package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// LaunchWithDebug 以调试模式后台启动 App
func LaunchWithDebug(appPath string, port int) error {
	args := []string{
		appPath,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = filepath.Dir(appPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	setSysProcAttr(cmd) // 平台特定的进程属性（detach）

	return cmd.Start()
}

// KillProcess 强制结束所有匹配的目标 App 进程并确保完全退出
func KillProcess(cfg AppConfig) {
	fmt.Printf("正在强制结束已运行的 %s 实例...\n", cfg.Name)
	KillApp(cfg)
	// 等待所有残留进程彻底退出，避免 SingleInstanceLock 抢占导致新实例自杀
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		app := DetectApp(cfg)
		if !app.Running {
			break
		}
	}
}

// fileExists 判断文件是否存在
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
