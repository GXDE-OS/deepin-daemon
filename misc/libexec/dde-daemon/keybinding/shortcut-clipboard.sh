#!/bin/bash
monitor=`ps -ef | grep dde-clipboard | grep -v grep | grep -v daemon | grep $(whoami) | wc -l `
if [ $monitor -eq 0 ]
then
	echo "Clipboard had exited, restart"
	setsid dde-clipboard &
	sleep 1.5
fi
dbus-send --print-reply --dest=org.deepin.dde.Clipboard1 /org/deepin/dde/Clipboard1 org.deepin.dde.Clipboard1.Toggle
