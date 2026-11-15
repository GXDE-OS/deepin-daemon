package grub2

import (
	"bytes"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	grubParamsFile = "/etc/default/grub"
	// 写入 GRUB_TERMINAL_OUTPUT=console 后 update-grub,GRUB 将使用纯文本
	// console,不再加载任何主题;删除该文件即可恢复主题
	grubConsoleCfgFile = "/etc/default/grub.d/grub-console.cfg"
)

const (
	grubBackground = "GRUB_BACKGROUND"
	grubDefault    = "GRUB_DEFAULT"
	grubGfxmode    = "GRUB_GFXMODE"
	grubTheme      = "GRUB_THEME"
	grubTimeout    = "GRUB_TIMEOUT"

	defaultGrubTheme      = "/boot/grub/themes/deepin/theme.txt"
	fallbackGrubTheme     = "/boot/grub/themes/deepin-fallback/theme.txt"
	defaultGrubDefault    = "0"
	defaultGrubDefaultInt = 0
	defaultGrubGfxMode    = "auto"
	defaultGrubTimeoutInt = 5
)

func decodeShellValue(in string) string {
	output, err := exec.Command("/bin/sh", "-c", "echo -n "+in).Output()
	if err != nil {
		// fallback
		return strings.Trim(in, "\"")
	}
	return string(output)
}

func getTimeout(params map[string]string) int {
	timeoutStr := decodeShellValue(params[grubTimeout])
	timeoutInt, err := strconv.Atoi(timeoutStr)
	if err != nil {
		return defaultGrubTimeoutInt
	}
	return timeoutInt
}

func getGfxMode(params map[string]string) (val string) {
	val = decodeShellValue(params[grubGfxmode])
	if val == "" {
		val = defaultGrubGfxMode
	}
	return
}

func getDefaultEntry(params map[string]string) (val string) {
	val = decodeShellValue(params[grubDefault])
	if val == "" {
		val = defaultGrubDefault
	}
	return
}

func getTheme(params map[string]string) string {
	return decodeShellValue(params[grubTheme])
}

func getGrubParamsContent(params map[string]string) []byte {
	keys := make(sort.StringSlice, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	keys.Sort()

	// write buf
	var buf bytes.Buffer
	buf.WriteString("# Written by " + dbusServiceName + "\n")
	for _, k := range keys {
		buf.WriteString(k + "=" + params[k] + "\n")
	}
	// if you want let the grub-mkconfig exit with error code,
	// uncomment the next line.
	//buf.WriteString("=\n")
	return buf.Bytes()
}

func writeGrubParams(params map[string]string) error {
	logger.Debug("write grub params")
	content := getGrubParamsContent(params)

	err := ioutil.WriteFile(grubParamsFile, content, 0644)
	if err != nil {
		return err
	}

	return nil
}

// grubConsoleCfgExists 返回主题是否已被禁用(grub-console.cfg 是否存在)
func grubConsoleCfgExists() bool {
	_, err := os.Stat(grubConsoleCfgFile)
	return err == nil
}

// setGrubConsoleCfg 写入或删除 grub-console.cfg 以禁用/恢复 grub 主题
func setGrubConsoleCfg(disabled bool) error {
	if disabled {
		dir := filepath.Dir(grubConsoleCfgFile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		return ioutil.WriteFile(grubConsoleCfgFile,
			[]byte("GRUB_TERMINAL_OUTPUT=console\n"), 0644)
	}
	err := os.Remove(grubConsoleCfgFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
