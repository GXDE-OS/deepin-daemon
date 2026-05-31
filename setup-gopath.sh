#!/bin/bash
set -e

GODIR=/tmp/godeepin/src

mkdir -p "$GODIR/pkg.deepin.io"

# 标准 Go 库
ln -sf /usr/share/gocode/src/github.com "$GODIR/github.com"
ln -sf /usr/share/gocode/src/golang.org "$GODIR/golang.org"
ln -sf /usr/share/gocode/src/gopkg.in   "$GODIR/gopkg.in"

# Deepin 旧路径映射
ln -sf /usr/share/gocode/src/github.com/linuxdeepin/go-gir          "$GODIR/pkg.deepin.io/gir"
ln -sf /usr/share/gocode/src/github.com/linuxdeepin/go-lib          "$GODIR/pkg.deepin.io/lib"
ln -sf /usr/share/gocode/src/github.com/linuxdeepin/go-dbus-factory "$GODIR/github.com/linuxdeepin/go-dbus-factory"

# daemon 源码
ln -sf /home/char/Desktop/Repositories/GXDE/deepin-daemon "$GODIR/pkg.deepin.io/dde"

echo "GOPATH 布局完成"
ls "$GODIR/pkg.deepin.io/"
