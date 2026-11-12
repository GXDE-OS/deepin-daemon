package network

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/linuxdeepin/go-dbus-factory/org.freedesktop.secrets"
	"pkg.deepin.io/dde/daemon/network/nm"
	"pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbusutil"
	"pkg.deepin.io/lib/strv"
)

const (
	nmSecretDialogBin              = "/usr/lib/deepin-daemon/dnetwork-secret-dialog"
	getSecretsFlagAllowInteraction = 0x1
	getSecretsFlagRequestNew       = 0x2
	getSecretsFlagUserRequested    = 0x4

	secretFlagNone          = 0 // save for all user
	secretFlagNoneStr       = "0"
	secretFlagAgentOwned    = 1 // save for me
	secretFlagAgentOwnedStr = "1"
	secretFlagAsk           = 2 // always ask
	secretFlagAskStr        = "2"
	secretFlagNotRequired   = 4 // no need password

	// keep keyring tags same with nm-applet
	keyringTagConnUUID    = "connection-uuid"
	keyringTagSettingName = "setting-name"
	keyringTagSettingKey  = "setting-key"
)

type saveSecretsTaskKey struct {
	connPath    dbus.ObjectPath
	settingName string
	id          uint64
}

type saveSecretsTask struct {
	process *os.Process
}

type SecretAgent struct {
	secretService       *secrets.Service
	secretSessionPath   dbus.ObjectPath
	defaultCollection   *secrets.Collection
	defaultCollectionMu sync.Mutex

	saveSecretsTasks      map[saveSecretsTaskKey]saveSecretsTask
	saveSecretsTasksMu    sync.Mutex
	nextSaveSecretsTaskID uint64

	methods *struct {
		GetSecrets        func() `in:"connection,connectionPath,settingName,hints,flags" out:"secrets"`
		CancelGetSecrets  func() `in:"connectionPath,settingName"`
		SaveSecrets       func() `in:"connection,connectionPath"`
		SaveSecretsDeepin func() `in:"connection,connectionPath"`
		DeleteSecrets     func() `in:"connection,connectionPath"`
	}
}

// addSaveSecretsTask 登记一个密码询问进程，返回其唯一 id。
// 同一个连接可能同时存在多个 GetSecrets 请求，不能用连接路径和 setting
// 名称作为唯一键，否则后一个请求会覆盖前一个，导致 CancelGetSecrets
// 杀错进程或漏杀。
func (sa *SecretAgent) addSaveSecretsTask(connPath dbus.ObjectPath,
	settingName string, process *os.Process) uint64 {
	sa.saveSecretsTasksMu.Lock()
	defer sa.saveSecretsTasksMu.Unlock()

	sa.nextSaveSecretsTaskID++
	id := sa.nextSaveSecretsTaskID
	sa.saveSecretsTasks[saveSecretsTaskKey{
		connPath:    connPath,
		settingName: settingName,
		id:          id,
	}] = saveSecretsTask{process: process}
	return id
}

func (sa *SecretAgent) removeSaveSecretsTask(connPath dbus.ObjectPath,
	settingName string, id uint64) {
	sa.saveSecretsTasksMu.Lock()
	delete(sa.saveSecretsTasks, saveSecretsTaskKey{
		connPath:    connPath,
		settingName: settingName,
		id:          id,
	})
	sa.saveSecretsTasksMu.Unlock()
}

// cancelSaveSecretsTasks 取消该连接该 setting 下所有仍在运行的密码询问进程。
func (sa *SecretAgent) cancelSaveSecretsTasks(connPath dbus.ObjectPath,
	settingName string) {
	sa.saveSecretsTasksMu.Lock()
	defer sa.saveSecretsTasksMu.Unlock()

	for key, task := range sa.saveSecretsTasks {
		if key.connPath != connPath || key.settingName != settingName {
			continue
		}
		if task.process != nil {
			logger.Debug("kill process", task.process.Pid)
			_ = task.process.Kill()
		}
	}
}

func (sa *SecretAgent) getDefaultCollection() (*secrets.Collection, error) {
	sa.defaultCollectionMu.Lock()
	defer sa.defaultCollectionMu.Unlock()

	if sa.defaultCollection != nil {
		return sa.defaultCollection, nil
	}

	collectionPath, err := sa.secretService.ReadAlias(0, "default")
	if err != nil {
		return nil, err
	}

	if collectionPath == "/" {
		return nil, errors.New("failed to get default collection path")
	}

	sessionBus, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}

	collectionObj, err := secrets.NewCollection(sessionBus, collectionPath)
	if err == nil {
		sa.defaultCollection = collectionObj
	}
	return collectionObj, err
}

func newSecretAgent(secServiceObj *secrets.Service) (*SecretAgent, error) {
	_, sessionPath, err := secServiceObj.OpenSession(0, "plain", dbus.MakeVariant(""))
	if err != nil {
		return nil, err
	}

	sa := &SecretAgent{}
	sa.secretSessionPath = sessionPath
	sa.secretService = secServiceObj
	sa.saveSecretsTasks = make(map[saveSecretsTaskKey]saveSecretsTask)
	logger.Debug("session path:", sessionPath)
	return sa, nil
}

func (sa *SecretAgent) deleteAll(uuid string) error {
	attributes := map[string]string{
		keyringTagConnUUID: uuid,
	}

	defaultCollection, err := sa.getDefaultCollection()
	if err != nil {
		return err
	}

	items, err := defaultCollection.SearchItems(0, attributes)
	if err != nil {
		return err
	}
	sessionBus, err := dbus.SessionBus()
	if err != nil {
		return err
	}

	for _, itemPath := range items {
		itemObj, err := secrets.NewItem(sessionBus, itemPath)
		if err != nil {
			continue
		}
		_, err = itemObj.Delete(0)
		if err != nil {
			logger.Warningf("delete item %q failed: %v\n", itemPath, err)
		}
	}
	return nil
}

func (sa *SecretAgent) getAll(uuid, settingName string) (map[string]string, error) {
	attributes := map[string]string{
		keyringTagConnUUID:    uuid,
		keyringTagSettingName: settingName,
	}
	defaultCollection, err := sa.getDefaultCollection()
	if err != nil {
		return nil, err
	}

	items, err := defaultCollection.SearchItems(0, attributes)
	if err != nil {
		return nil, err
	}

	secretsData, err := sa.secretService.GetSecrets(0, items, sa.secretSessionPath)
	if err != nil {
		return nil, err
	}

	sessionBus, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	var result = make(map[string]string)
	for itemPath, itemSecret := range secretsData {
		itemObj, err := secrets.NewItem(sessionBus, itemPath)
		if err != nil {
			return nil, err
		}

		attributes, _ := itemObj.Attributes().Get(0)
		settingKey := attributes[keyringTagSettingKey]
		if settingKey != "" {
			result[settingKey] = string(itemSecret.Value)
		}
	}
	return result, nil
}

func (sa *SecretAgent) delete(uuid, settingName, settingKey string) error {
	attributes := map[string]string{
		keyringTagConnUUID:    uuid,
		keyringTagSettingName: settingName,
		keyringTagSettingKey:  settingKey,
	}
	defaultCollection, err := sa.getDefaultCollection()
	if err != nil {
		return err
	}

	items, err := defaultCollection.SearchItems(0, attributes)
	if err != nil {
		return err
	}

	if len(items) == 0 {
		return nil
	}

	logger.Debugf("delete uuid: %q, setting name: %q, setting key: %q\n",
		uuid, settingName, settingKey)

	sessionBus, err := dbus.SessionBus()
	if err != nil {
		return err
	}
	item := items[0]
	itemObj, err := secrets.NewItem(sessionBus, item)
	if err != nil {
		return err
	}
	_, err = itemObj.Delete(0)
	return err
}

func (sa *SecretAgent) set(label, uuid, settingName, settingKey, value string) error {
	logger.Debugf("set label: %q, uuid: %q, setting name: %q, setting key: %q, value: %q",
		label, uuid, settingName, settingKey, value)
	itemSecret := secrets.Secret{
		Session:     sa.secretSessionPath,
		Value:       []byte(value),
		ContentType: "text/plain",
	}

	properties := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label": dbus.MakeVariant(label),
		"org.freedesktop.Secret.Item.Type":  dbus.MakeVariant("org.freedesktop.Secret.Generic"),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(map[string]string{
			keyringTagConnUUID:    uuid,
			keyringTagSettingName: settingName,
			keyringTagSettingKey:  settingKey,
		}),
	}

	defaultCollection, err := sa.getDefaultCollection()
	if err != nil {
		return err
	}
	_, _, err = defaultCollection.CreateItem(0, properties, itemSecret, true)
	return err
}

func (*SecretAgent) GetInterfaceName() string {
	return "org.freedesktop.NetworkManager.SecretAgent"
}

type getSecretsRequest struct {
	ConnId      string   `json:"connId"`
	ConnType    string   `json:"connType"`
	ConnUUID    string   `json:"connUUID"`
	VpnService  string   `json:"vpnService"`
	SettingName string   `json:"settingName"`
	Secrets     []string `json:"secrets"`
}

type getSecretsReply struct {
	Secrets []string `json:"secrets"`
}

func (sa *SecretAgent) askPasswords(connPath dbus.ObjectPath,
	connectionData map[string]map[string]dbus.Variant,
	connUUID, settingName string, settingKeys []string) (map[string]string, error) {

	logger.Debugf("askPasswords settingName: %v, settingKeys: %v",
		settingName, settingKeys)
	connId, _ := getConnectionDataString(connectionData, "connection", "id")

	connType, _ := getConnectionDataString(connectionData, "connection", "type")

	vpnService, _ := getConnectionDataString(connectionData, "vpn", "service")

	var req getSecretsRequest
	req.ConnId = connId
	req.ConnType = connType
	req.ConnUUID = connUUID
	req.VpnService = vpnService
	req.SettingName = settingName
	req.Secrets = settingKeys
	logger.Warning(req)
	reqJSON, err := json.Marshal(&req)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(nmSecretDialogBin)
	cmd.Stdin = bytes.NewReader(reqJSON)
	var cmdOutBuf bytes.Buffer
	cmd.Stdout = &cmdOutBuf
	err = cmd.Start()
	if err != nil {
		return nil, err
	}
	taskID := sa.addSaveSecretsTask(connPath, settingName, cmd.Process)
	waitErr := cmd.Wait()
	sa.removeSaveSecretsTask(connPath, settingName, taskID)

	// dnetwork-secret-dialog 先把密码结果写到 stdout，之后再退出进程。它在
	// 退出时可能因 libdtkwidget 全局析构中的堆损坏而 SIGABRT (core dumped)，
	// 此时 stdout 中的密码仍然完整有效。所以不能仅凭子进程的退出状态就丢弃
	// 结果，必须先尝试解析 stdout，否则 NetworkManager 会收到空 secrets 并
	// 以 no-secrets 让连接失败。
	secretValues, parseErr := parseSecretsReply(cmdOutBuf.Bytes(), len(settingKeys))
	if parseErr != nil {
		if waitErr != nil {
			return nil, fmt.Errorf("secret dialog failed: %v, parse stdout failed: %v",
				waitErr, parseErr)
		}
		return nil, parseErr
	}
	if waitErr != nil {
		logger.Warningf("secret dialog exited abnormally but returned valid secrets: %v", waitErr)
	}

	result := make(map[string]string, len(settingKeys))
	for i, key := range settingKeys {
		result[key] = secretValues[i]
	}
	return result, nil
}

// parseSecretsReply 从密码对话框的输出中提取密码列表。
// 对话框会把 DTK 调试日志和格式化（多行）的 JSON 一起写到 stdout，日志可能
// 出现在 JSON 之前或之后，用贪婪正则匹配整个输出很容易解析失败。这里逐个
// 尝试以 '{' 开头的位置，用 Decoder 只解析第一个完整的 JSON 对象（自动忽略
// 其后的日志），并要求 secrets 数量与请求的密钥数量一致。
func parseSecretsReply(output []byte, expected int) ([]string, error) {
	lastErr := errors.New("secretAgent.askPasswords: no valid secrets reply in stdout")
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		var reply getSecretsReply
		if err := json.NewDecoder(bytes.NewReader(output[i:])).Decode(&reply); err != nil {
			lastErr = err
			continue
		}
		if len(reply.Secrets) != expected {
			lastErr = fmt.Errorf("secretAgent.askPasswords: length not equal, got %d, want %d",
				len(reply.Secrets), expected)
			continue
		}
		return reply.Secrets, nil
	}
	// 不要把 stdout 写进日志，其中可能包含用户输入的密码。
	logger.Errorf("failed to parse secret dialog output, length: %d, err: %v",
		len(output), lastErr)
	return nil, lastErr
}

func (sa *SecretAgent) GetSecrets(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath, settingName string, hints []string, flags uint32) (
	secretsData map[string]map[string]dbus.Variant, busErr *dbus.Error) {
	var err error
	secretsData, err = sa.getSecrets(connectionData, connectionPath, settingName, hints, flags)
	if err != nil {
		return nil, dbusutil.ToError(err)
	}

	logger.Debugf("secretsData: %#v", secretsData)
	return secretsData, nil
}

func getSecretFlagsKeyName(key string) string {
	if strings.HasPrefix(key, "wep-key") {
		num, err := strconv.Atoi(string(key[len(key)-1]))
		if err == nil && 0 <= num && num <= 3 {
			// num in range [0,3]
			return "wep-key-flags"
		}
	}
	return key + "-flags"
}

// 根据当前连接设置，找出必要的密码key。
func isMustAsk(data connectionData, settingName, secretKey string) bool {
	mgmt := getSettingWirelessSecurityKeyMgmt(data)
	switch settingName {
	case nm.NM_SETTING_WIRELESS_SECURITY_SETTING_NAME:
		wepTxKeyIdx := getSettingWirelessSecurityWepTxKeyidx(data)
		switch mgmt {
		// WPA3-Personal (SAE)、WPA2/WPA3 混合以及 802.11r 快速漫游的连接
		// 同样使用 psk，否则这类网络永远不会被询问密码，NetworkManager 会
		// 因为收不到 secrets 而直接把连接置为失败。
		case "wpa-psk", "sae", "wpa-psk-sha256", "ft-psk", "ft-sae":
			if secretKey == "psk" {
				return true
			}

		case "none":
			if secretKey == "wep-key0" && wepTxKeyIdx == 0 {
				return true
			} else if secretKey == "wep-key1" && wepTxKeyIdx == 1 {
				return true
			} else if secretKey == "wep-key2" && wepTxKeyIdx == 2 {
				return true
			} else if secretKey == "wep-key3" && wepTxKeyIdx == 3 {
				return true
			}
		}

	case nm.NM_SETTING_802_1X_SETTING_NAME:
		eap := getSetting8021xEap(data)
		var eap0 string
		if len(eap) >= 1 {
			eap0 = eap[0]
		}
		switch eap0 {
		case "md5", "fast", "ttls", "peap", "leap":
			if secretKey == "password" {
				return true
			}
		case "tls":
			if secretKey == "private-key-password" {
				return true
			}
		}

	}

	return false
}

func (sa *SecretAgent) getSecrets(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath, settingName string, hints []string, flags uint32) (
	secretsData map[string]map[string]dbus.Variant, err error) {

	logger.Debug("call getSecrets")

	var allowInteraction bool
	var requestNew bool
	if flags&getSecretsFlagAllowInteraction != 0 {
		logger.Debug("allow interaction")
		allowInteraction = true
	}
	if flags&getSecretsFlagRequestNew != 0 {
		logger.Debug("request new")
		requestNew = true
	}

	logger.Debug("connection path:", connectionPath)
	logger.Debug("setting Name:", settingName)
	logger.Debug("hints:", hints)
	logger.Debug("flags:", flags)

	printConnectionData(connectionData)

	connUUID, ok := getConnectionDataString(connectionData, "connection", "uuid")
	if !ok {
		return nil, errors.New("not found connection uuid")
	}

	logger.Debug("uuid:", connUUID)
	secretsData = make(map[string]map[string]dbus.Variant)
	setting := make(map[string]dbus.Variant)
	secretsData[settingName] = setting

	if settingName == "vpn" {
		vpnSecretsData := make(map[string]string)

		vpnSecretsMap, ok := getConnectionDataMapStrStr(connectionData, "vpn",
			"secrets")
		if ok {
			for key, value := range vpnSecretsMap {
				vpnSecretsData[key] = value
			}
		}
		vpnDataMap, _ := getConnectionDataMapStrStr(connectionData, "vpn",
			"data")

		var askItems []string
		for _, secretKey := range vpnSecretKeys {
			secretFlag := vpnDataMap[getSecretFlagsKeyName(secretKey)]
			if secretFlag == secretFlagAskStr {
				logger.Debug("ask for password", settingName, secretKey)
				askItems = append(askItems, secretKey)
			}
		}

		if allowInteraction && len(askItems) > 0 {
			resultAsk, err := sa.askPasswords(connectionPath, connectionData, connUUID,
				settingName, askItems)
			if err != nil {
				logger.Debug("waring askPasswords error:", err)
				return nil, err
			}
			for key, value := range resultAsk {
				vpnSecretsData[key] = value
			}
		}

		resultSaved, err := sa.getAll(connUUID, settingName)
		if err != nil {
			return nil, err
		}
		logger.Debug("getAll resultSaved:", resultSaved)

		for key, value := range resultSaved {
			if _, ok := vpnSecretsData[key]; !ok {
				vpnSecretsData[key] = value
			} else {
				logger.Debug("not override key", key)
			}
		}

		setting["secrets"] = dbus.MakeVariant(vpnSecretsData)

	} else if secretKeys, ok := secretSettingKeys[settingName]; ok {

		var askItems []string
		for _, secretKey := range secretKeys {
			secretFlags, _ := getConnectionDataUint32(connectionData, settingName,
				getSecretFlagsKeyName(secretKey))

			if secretFlags == secretFlagAsk {
				if allowInteraction && isMustAsk(connectionData, settingName, secretKey) {
					askItems = append(askItems, secretKey)
				}
			} else if secretFlags == secretFlagNone {
				secretStr, _ := getConnectionDataString(connectionData, settingName,
					secretKey)

				if requestNew {
					secretStr = ""
				}

				if secretStr != "" {
					setting[secretKey] = dbus.MakeVariant(secretStr)
				} else if allowInteraction &&
					isMustAsk(connectionData, settingName, secretKey) {
					askItems = append(askItems, secretKey)
				}
			}
		}

		var askErr error
		if allowInteraction && len(askItems) > 0 {
			resultAsk, err := sa.askPasswords(connectionPath, connectionData, connUUID,
				settingName, askItems)
			if err != nil {
				logger.Warning("askPasswords error:", err)
				askErr = err
			} else {
				for key, value := range resultAsk {
					setting[key] = dbus.MakeVariant(value)
				}
			}
		}

		resultSaved, err := sa.getAll(connUUID, settingName)
		if err != nil {
			return nil, err
		}
		logger.Debugf("getAll resultSaved: %#v", resultSaved)

		for key, value := range resultSaved {
			secretFlags, _ := getConnectionDataUint32(connectionData, settingName,
				getSecretFlagsKeyName(key))
			if secretFlags == secretFlagAgentOwned {
				setting[key] = dbus.MakeVariant(value)
				continue
			}
			// 密码询问失败时退回 keyring 中保存的密码，避免把空的 secrets
			// 当作成功结果返回给 NetworkManager。
			if askErr != nil && strv.Strv(askItems).Contains(key) {
				if _, ok := setting[key]; !ok {
					setting[key] = dbus.MakeVariant(value)
				}
			}
		}

		if askErr != nil {
			for _, key := range askItems {
				if _, ok := setting[key]; !ok {
					// 确实拿不到密码，明确向 NetworkManager 报错，而不是
					// 静默返回空结果造成 no-secrets/密码错误的误判。
					return nil, askErr
				}
			}
			logger.Warning("askPasswords failed, use saved secrets instead:", askErr)
		}
	}
	return
}

func printConnectionData(data map[string]map[string]dbus.Variant) {
	for settingName, setting := range data {
		for key, value := range setting {
			logger.Debugf("> %s.%s: %v", settingName, key, value)
		}
	}
}

func (sa *SecretAgent) CancelGetSecrets(connectionPath dbus.ObjectPath, settingName string) *dbus.Error {
	logger.Debug("call CancelGetSecrets")

	logger.Debug("connection path:", connectionPath)
	logger.Debug("setting name:", settingName)

	sa.cancelSaveSecretsTasks(connectionPath, settingName)
	return nil
}

type settingItem struct {
	settingName string
	settingKey  string
	value       string
	label       string
}

func getConnectionDataVariant(connectionData map[string]map[string]dbus.Variant,
	settingName, settingKey string) (dbus.Variant, bool) {

	setting, ok := connectionData[settingName]
	if !ok {
		return dbus.Variant{}, false
	}
	value, ok := setting[settingKey]
	if !ok {
		return dbus.Variant{}, false
	}
	return value, true
}

func getConnectionData(connectionData map[string]map[string]dbus.Variant,
	settingName, settingKey string) (interface{}, bool) {

	variant, ok := getConnectionDataVariant(connectionData, settingName, settingKey)
	if !ok {
		return nil, false
	}
	return variant.Value(), true
}

func getConnectionDataString(connectionData map[string]map[string]dbus.Variant,
	settingName, settingKey string) (string, bool) {
	val, ok := getConnectionData(connectionData, settingName, settingKey)
	if ok {
		valStr, ok := val.(string)
		if ok {
			return valStr, true
		}
	}
	return "", false
}

func getConnectionDataMapStrStr(connectionData map[string]map[string]dbus.Variant,
	settingName, settingKey string) (map[string]string, bool) {

	val, ok := getConnectionData(connectionData, settingName, settingKey)
	if ok {
		valMap, ok := val.(map[string]string)
		if ok {
			return valMap, true
		}
	}
	return nil, false
}

func getConnectionDataUint32(connectionData map[string]map[string]dbus.Variant,
	settingName, settingKey string) (uint32, bool) {

	val, ok := getConnectionData(connectionData, settingName, settingKey)
	if ok {
		valUint, ok := val.(uint32)
		if ok {
			return valUint, true
		}
	}
	return 0, false
}

var secretSettingKeys = map[string][]string{
	"802-11-wireless-security": {"psk", "wep-key0", "wep-key1", "wep-key2", "wep-key3",
		"leap-password"},
	"802-1x": {"password", "password-raw", "ca-cert-password",
		"client-cert-password", "phase2-ca-cert-password", "phase2-client-cert-password",
		"private-key-password", "phase2-private-key-password", "pin"},
	// temporarily not supported password-raw
	"pppoe": {"password"},
	"gsm":   {"password", "pin"},
	"cdma":  {"password"},
}

var vpnSecretKeys = []string{
	"password", "proxy-password", "IPSec secret", "Xauth password",
}

func (sa *SecretAgent) SaveSecretsDeepin(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath) *dbus.Error {
	err := sa.saveSecrets(connectionData, connectionPath)
	return dbusutil.ToError(err)
}

func (sa *SecretAgent) SaveSecrets(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath) *dbus.Error {
	err := sa.saveSecrets(connectionData, connectionPath)
	return dbusutil.ToError(err)
}

func (sa *SecretAgent) saveSecrets(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath) error {
	logger.Debug("call saveSecrets")

	printConnectionData(connectionData)
	logger.Debug("connection path:", connectionPath)

	connUUID, ok := getConnectionDataString(connectionData, "connection",
		"uuid")
	if !ok {
		return dbusutil.ToError(errors.New("not found connection uuid"))
	}
	logger.Debug("uuid:", connUUID)
	connId, _ := getConnectionDataString(connectionData, "connection", "id")
	logger.Debug("conn id:", connId)

	vpnServiceType, _ := getConnectionDataString(connectionData, "vpn", "service-type")
	dotLastIdx := strings.LastIndex(vpnServiceType, ".")
	if dotLastIdx != -1 {
		vpnServiceType = vpnServiceType[dotLastIdx+1:]
	}

	var arr []settingItem

	for settingName, setting := range connectionData {

		if settingName == "vpn" {
			var vpnDataMap map[string]string
			vpnData, ok := setting["data"]
			if ok {
				vpnDataMap, _ = vpnData.Value().(map[string]string)
				logger.Debug("vpn.data map:", vpnDataMap)
			}

			secret, ok := setting["secrets"]
			if ok {
				logger.Debug("vpn.secret value:", secret)
				secretMap, ok := secret.Value().(map[string]string)
				if ok {
					for key, value := range secretMap {
						secretFlags := vpnDataMap[getSecretFlagsKeyName(key)]

						if secretFlags == secretFlagAgentOwnedStr {
							label := fmt.Sprintf("VPN password secret for %s/%s/%s",
								connId, vpnServiceType, key)
							arr = append(arr, settingItem{
								settingName: settingName,
								settingKey:  key,
								value:       value,
								label:       label,
							})
						}
					}
				}
			}
			continue
		}

		secretKeys := secretSettingKeys[settingName]
		for key, value := range setting {
			if strv.Strv(secretKeys).Contains(key) {
				// key is secret key

				secretFlags, _ := getConnectionDataUint32(connectionData,
					settingName, getSecretFlagsKeyName(key))
				if secretFlags != secretFlagAgentOwned {
					// not agent owned
					continue
				}

				valueStr, ok := value.Value().(string)
				if ok {
					arr = append(arr, settingItem{
						settingName: settingName,
						settingKey:  key,
						value:       valueStr,
					})
				}
			}
		}
	}

	for _, item := range arr {
		label := item.label
		if label == "" {
			label = fmt.Sprintf("Network secret for %s/%s/%s", connId,
				item.settingName, item.settingKey)
		}
		sa.set(label, connUUID, item.settingName, item.settingKey, item.value)
	}

	// delete
	for settingName, secretKeys := range secretSettingKeys {
		for _, secretKey := range secretKeys {
			secretFlags, _ := getConnectionDataUint32(connectionData, settingName,
				getSecretFlagsKeyName(secretKey))

			if secretFlags != secretFlagAgentOwned {
				sa.delete(connUUID, settingName, secretKey)
			}
		}
	}

	vpnData, ok := getConnectionData(connectionData, "vpn", "data")
	if ok {
		vpnDataMap, ok := vpnData.(map[string]string)
		if ok {
			for _, secretKey := range vpnSecretKeys {
				secretFlags := vpnDataMap[getSecretFlagsKeyName(secretKey)]
				if secretFlags != secretFlagAgentOwnedStr {
					sa.delete(connUUID, "vpn", secretKey)
				}
			}
		}
	}

	return nil
}

func (sa *SecretAgent) DeleteSecrets(connectionData map[string]map[string]dbus.Variant,
	connectionPath dbus.ObjectPath) *dbus.Error {
	logger.Debug("call DeleteSecrets")
	printConnectionData(connectionData)

	connUUID, ok := getConnectionDataString(connectionData, "connection",
		"uuid")
	if !ok {
		return dbusutil.ToError(errors.New("not found connection uuid"))
	}

	err := sa.deleteAll(connUUID)
	return dbusutil.ToError(err)
}

func (*SecretAgentSession) GetSystemBusName() (string, *dbus.Error) {
	sysBus, err := dbus.SystemBus()
	if err != nil {
		return "", dbusutil.ToError(err)
	}
	return sysBus.Names()[0], nil
}

type SecretAgentSession struct {
	methods *struct {
		GetSystemBusName func() `out:"name"`
	}
}

func (*SecretAgentSession) GetInterfaceName() string {
	return "com.deepin.daemon.Network.SecretAgent"
}
