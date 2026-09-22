package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// CDPTarget 调试目标页面
type CDPTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpIDCounter CDP 消息 ID 计数器
var cdpIDCounter atomic.Int64

// cdpCall 发送 CDP 命令并等待响应
func cdpCall(conn *websocket.Conn, method string, params map[string]interface{}) (map[string]interface{}, error) {
	id := cdpIDCounter.Add(1)
	payload := map[string]interface{}{
		"id":     id,
		"method": method,
		"params": params,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return nil, fmt.Errorf("发送 CDP 消息失败: %w", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("接收 CDP 响应失败: %w", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(msg, &resp); err != nil {
			continue
		}
		if respID, ok := resp["id"]; ok {
			matched := false
			switch v := respID.(type) {
			case float64:
				matched = int64(v) == id
			case string:
				matched = v == strconv.FormatInt(id, 10)
			}
			if matched {
				if err := cdpResponseError(resp, method); err != nil {
					return nil, err
				}
				return resp, nil
			}
		}
	}
	return nil, fmt.Errorf("等待 %s 响应超时", method)
}

// cdpResponseError 将 CDP 协议错误和 Runtime.evaluate 的 JS 异常统一转换为 Go 错误。
// CDP 即使执行脚本抛异常，WebSocket 命令本身通常仍会返回成功，因此不能只检查 cdpCall 的网络错误。
func cdpResponseError(resp map[string]interface{}, method string) error {
	if protocolErr, ok := resp["error"].(map[string]interface{}); ok {
		message, _ := protocolErr["message"].(string)
		if message == "" {
			message = fmt.Sprint(protocolErr)
		}
		return fmt.Errorf("CDP %s 失败: %s", method, message)
	}

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		return nil
	}
	if exception, ok := result["exceptionDetails"].(map[string]interface{}); ok {
		description, _ := exception["text"].(string)
		if description == "" {
			if value, ok := exception["exception"].(map[string]interface{}); ok {
				description, _ = value["description"].(string)
			}
		}
		if description == "" {
			description = fmt.Sprint(exception)
		}
		return fmt.Errorf("Runtime.evaluate 执行异常: %s", description)
	}
	return nil
}

// isTargetPage 判断目标是否为需汉化页面
func isTargetPage(url, appName string) bool {
	urlLower := strings.ToLower(url)
	if strings.Contains(strings.ToLower(appName), "ide") {
		return strings.Contains(urlLower, "workbench.html") ||
			strings.Contains(urlLower, "workbench-jetski-agent.html")
	}
	return strings.HasPrefix(urlLower, "data:text/html") ||
		strings.Contains(urlLower, "127.0.0.1") ||
		strings.Contains(urlLower, "localhost")
}

// getTargetTitle 获取页面标题
func getTargetTitle(title, rawURL string) string {
	if title != "" {
		if len(title) > 100 {
			return title[:97] + "..."
		}
		return title
	}
	urlLower := strings.ToLower(rawURL)
	if strings.HasPrefix(urlLower, "data:text/html") {
		return "首屏加载页"
	}
	if strings.Contains(urlLower, "workbench-jetski-agent.html") {
		return "Jetski Agent 面板"
	}
	if strings.Contains(urlLower, "workbench.html") {
		return "主窗口"
	}
	if strings.Contains(urlLower, "127.0.0.1") || strings.Contains(urlLower, "localhost") {
		return "应用主页面"
	}
	if len(rawURL) > 100 {
		return rawURL[:97] + "..."
	}
	if rawURL != "" {
		return rawURL
	}
	return "主窗口"
}

// CheckPort 获取可注入的调试目标列表
func CheckPort(port int) []CDPTarget {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var targets []CDPTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil
	}

	ignoredTypes := map[string]bool{
		"worker": true, "service_worker": true,
		"shared_worker": true, "background_page": true,
	}
	var result []CDPTarget
	for _, t := range targets {
		if !ignoredTypes[t.Type] && t.WebSocketDebuggerURL != "" {
			result = append(result, t)
		}
	}
	return result
}

// checkTargetInjectedOnConn 检查当前连接对应的页面文档是否已注入补丁
func checkTargetInjectedOnConn(conn *websocket.Conn) (bool, error) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := cdpCall(conn, "Runtime.evaluate", map[string]interface{}{
		"expression":    "window.__antigravityZhPatchInstalled === true && window.__observedDocument === document",
		"returnByValue": true,
	})
	if err != nil {
		return false, err
	}
	if result, ok := resp["result"].(map[string]interface{}); ok {
		if valObj, ok := result["result"].(map[string]interface{}); ok {
			if val, ok := valObj["value"].(bool); ok {
				return val, nil
			}
		}
	}
	return false, nil
}

// EnsureTargetInjected 在单条 WebSocket 连接中完成检测、脚本注入与复核。
// 避免为每次操作频繁建立/断开短连接，彻底杜绝 Windows ConnectEx 端口耗尽 (WSAEADDRINUSE 10048) 冲突。
// 返回值：(injected bool, isPermanentErr bool, err error)
func EnsureTargetInjected(target CDPTarget, overlaySource string) (bool, bool, error) {
	if target.WebSocketDebuggerURL == "" {
		return false, true, fmt.Errorf("webSocketDebuggerUrl 为空")
	}

	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.Dial(target.WebSocketDebuggerURL, nil)
	if err != nil {
		// 对端返回 404/500（如 Splash 页面关闭后 No such target id）或明确拒绝，代表目标已销毁，无需重试
		isPermanent := false
		if resp != nil && (resp.StatusCode == 404 || resp.StatusCode == 500) {
			isPermanent = true
		} else if strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "No such target") {
			isPermanent = true
		}
		return false, isPermanent, fmt.Errorf("连接 WebSocket 失败: %w", err)
	}
	defer conn.Close()

	// 1. 在同一个连接上先复核是否已完成注入
	injected, err := checkTargetInjectedOnConn(conn)
	if err == nil && injected {
		return true, false, nil
	}

	// 2. 启用 Page 域支持
	_, _ = cdpCall(conn, "Page.enable", nil)

	// 3. 注册新文档自动注入脚本（页面内部跳转、刷新后均自动生效）
	if _, err := cdpCall(conn, "Page.addScriptToEvaluateOnNewDocument", map[string]interface{}{
		"source": overlaySource,
	}); err != nil {
		return false, false, fmt.Errorf("监听页面刷新失败: %w", err)
	}

	// 4. 当前页面立即执行注入
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	evalResp, err := cdpCall(conn, "Runtime.evaluate", map[string]interface{}{
		"expression":   overlaySource,
		"awaitPromise": false,
	})
	if err != nil {
		return false, false, fmt.Errorf("Runtime.evaluate 失败: %w", err)
	}
	if err := cdpResponseError(evalResp, "Runtime.evaluate"); err != nil {
		return false, false, err
	}

	// 5. 在同一连接上直接复核安装状态
	verified, _ := checkTargetInjectedOnConn(conn)
	if !verified {
		return false, false, fmt.Errorf("注入后页面未确认汉化脚本已安装")
	}

	return true, false, nil
}

// IsTargetInjected 检测页面当前文档是否真正已注入汉化补丁
func IsTargetInjected(target CDPTarget) bool {
	if target.WebSocketDebuggerURL == "" {
		return false
	}
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(target.WebSocketDebuggerURL, nil)
	if err != nil {
		return false
	}
	defer conn.Close()

	injected, _ := checkTargetInjectedOnConn(conn)
	return injected
}

// InjectTarget 向目标页面注入脚本（兼容包装）
func InjectTarget(target CDPTarget, overlaySource string, injectedSet map[string]bool, mu *sync.Mutex) error {
	injected, _, err := EnsureTargetInjected(target, overlaySource)
	if injected && target.ID != "" && mu != nil {
		mu.Lock()
		injectedSet[target.ID] = true
		mu.Unlock()
	}
	return err
}

// WaitForPort 等待调试端口就绪
func WaitForPort(port int, maxWaitMs int) bool {
	start := time.Now()
	limit := time.Duration(maxWaitMs) * time.Millisecond
	for time.Since(start) < limit {
		targets := CheckPort(port)
		if len(targets) > 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// Run 启动并监视目标应用
func Run(cfg AppConfig, overlaySource string) {
	injectedSet := make(map[string]bool)
	var mu sync.Mutex

	app := DetectApp(cfg)

	// 已开启调试端口直接进入监视
	targets := CheckPort(app.Port)
	if len(targets) > 0 {
		fmt.Printf("检测到 %s 调试端口已开启，直接进入监视模式...\n", app.Name)
		Watch(cfg, overlaySource, injectedSet, &mu)
		return
	}

	// 进程未开启调试端口则重启
	if app.Running {
		fmt.Printf("[提示] 检测到 %s 正在运行，但未开启调试端口。正在重新拉起...\n", app.Name)
		KillProcess(cfg)
		time.Sleep(1500 * time.Millisecond)
	}

	// 确定启动路径
	app = DetectApp(cfg)
	targetPath := app.Path
	if targetPath == "" {
		switch len(app.AllPaths) {
		case 0:
			fmt.Printf("[错误] 未能在本机找到 %s 的安装路径。\n", app.Name)
			return
		case 1:
			targetPath = app.AllPaths[0]
		default:
			fmt.Printf("\n检测到本机存在多个 %s 安装实例，请选择启动哪一个：\n", app.Name)
			for i, p := range app.AllPaths {
				fmt.Printf(" [%d] %s\n", i+1, p)
			}
			var choice int
			fmt.Printf("请选择 (1-%d, 默认 1): ", len(app.AllPaths))
			fmt.Scan(&choice)
			if choice < 1 || choice > len(app.AllPaths) {
				choice = 1
			}
			targetPath = app.AllPaths[choice-1]
		}
	}

	// 启动并监视
	fmt.Printf("正在以调试模式启动 %s: %s ...\n", app.Name, targetPath)
	if err := LaunchWithDebug(targetPath, app.Port); err != nil {
		fmt.Printf("[错误] 启动失败: %v\n", err)
		return
	}
	fmt.Printf("正在等待 %s 调试端口就绪并建立监视...\n", app.Name)
	if WaitForPort(app.Port, 20000) {
		fmt.Printf("[成功] %s 调试接口已就绪。\n", app.Name)
	} else {
		fmt.Printf("[警告] 等待 %s 调试端口超时，尝试直接连接...\n", app.Name)
	}

	Watch(cfg, overlaySource, injectedSet, &mu)
}

// getBrowserWSURL 获取浏览器级 WebSocket 调试 URL
func getBrowserWSURL(port int) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP 状态码错误: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var versionInfo map[string]string
	if err := json.Unmarshal(body, &versionInfo); err != nil {
		return "", err
	}
	wsURL := versionInfo["webSocketDebuggerUrl"]
	if wsURL == "" {
		return "", fmt.Errorf("webSocketDebuggerUrl 为空")
	}
	return wsURL, nil
}

// Watch 基于 CDP 监听并自动注入
func Watch(cfg AppConfig, overlaySource string, injectedSet map[string]bool, mu *sync.Mutex) {
	fmt.Printf("启动 %s 汉化监视模式 (CDP 事件驱动型)...\n", cfg.Name)

	var wsURL string
	var err error

	// 获取调试地址
	for i := 0; i < 50; i++ {
		wsURL, err = getBrowserWSURL(cfg.Port)
		if err == nil && wsURL != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err != nil || wsURL == "" {
		fmt.Printf("[错误] 无法连接到 %s 调试接口: %v\n", cfg.Name, err)
		return
	}

	// 建立调试连接
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		fmt.Printf("[错误] 建立 CDP 事件长连接失败: %v\n", err)
		return
	}
	defer conn.Close()

	// 开启 Target 发现
	discoverCmd := map[string]interface{}{
		"id":     1,
		"method": "Target.setDiscoverTargets",
		"params": map[string]bool{
			"discover": true,
		},
	}
	discoverJSON, _ := json.Marshal(discoverCmd)
	if err := conn.WriteMessage(websocket.TextMessage, discoverJSON); err != nil {
		fmt.Printf("[错误] 发送 Target.setDiscoverTargets 失败: %v\n", err)
		return
	}

	fmt.Println("[成功] 汉化监视连接已建立，进入事件驱动注入状态。")
	fmt.Println("[提示] 请保持本控制台窗口运行以维持汉化注入；按 Ctrl+C 可随时退出。")

	// 目标注入逻辑
	injectionPending := make(map[string]bool)
	injectIfMatch := func(target CDPTarget, rawURL, triggerType string) {
		if target.ID == "" || !isTargetPage(rawURL, cfg.Name) {
			return
		}

		// 同一页面可能同时收到 targetCreated、targetInfoChanged 和主动初检，
		// 只允许一个注入任务运行；若已成功注入则无需重复注入。
		mu.Lock()
		if injectedSet[target.ID] || injectionPending[target.ID] {
			mu.Unlock()
			return
		}
		injectionPending[target.ID] = true
		mu.Unlock()

		go func() {
			defer func() {
				mu.Lock()
				delete(injectionPending, target.ID)
				mu.Unlock()
			}()

			const maxAttempts = 3
			var lastErr error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				// 单连接复用：一次性完成检查、Page.enable、addScriptToEvaluateOnNewDocument、evaluate 与复核
				injected, isPermanent, err := EnsureTargetInjected(target, overlaySource)
				if injected {
					mu.Lock()
					injectedSet[target.ID] = true
					mu.Unlock()
					title := getTargetTitle(target.Title, rawURL)
					fmt.Printf("[%s] 捕获到目标页面，成功注入汉化: %s (ID: %s)\n", triggerType, title, target.ID)
					return
				}

				// 若目标已被 Electron 销毁（如 Splash 首屏页面关闭），属于永久性状态，直接终止重试
				if isPermanent {
					return
				}

				lastErr = err
				if attempt < maxAttempts {
					time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
				}
			}
			if lastErr != nil {
				fmt.Printf("[警告] 页面注入失败（已重试 %d 次，后续页面事件或后台巡检将自动补刀）: %v (ID: %s)\n", maxAttempts, lastErr, target.ID)
			}
		}()
	}

	// 初始目标注入
	initialTargets := CheckPort(cfg.Port)
	for _, it := range initialTargets {
		injectIfMatch(it, it.URL, "主动初检")
	}

	// 启动后台 Watchdog 巡检器（彻底解决启动瞬间竞态、偶发残留英文版、页面重新导航问题）
	stopWatchdog := make(chan struct{})
	defer close(stopWatchdog)

	go func() {
		ticker := time.NewTicker(2000 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopWatchdog:
				return
			case <-ticker.C:
				targets := CheckPort(cfg.Port)
				for _, t := range targets {
					if !isTargetPage(t.URL, cfg.Name) {
						continue
					}

					mu.Lock()
					alreadyInjected := injectedSet[t.ID]
					isPending := injectionPending[t.ID]
					mu.Unlock()

					// 兜底自愈：若发现符合条件的存活页面尚未注入且未在注入队列中，立即自动补刀
					if !alreadyInjected && !isPending {
						injectIfMatch(t, t.URL, "巡检自愈")
					} else if alreadyInjected && !isPending {
						// 复核已注入页面：若用户刷新导致文档重置（__observedDocument 失效），重新补刀
						go func(target CDPTarget) {
							if !IsTargetInjected(target) {
								mu.Lock()
								delete(injectedSet, target.ID)
								mu.Unlock()
								injectIfMatch(target, target.URL, "巡检自愈")
							}
						}(t)
					}
				}
			}
		}
	}()

	// 事件监听
	type TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		Title    string `json:"title"`
		URL      string `json:"url"`
	}
	type CDPNotification struct {
		Method string `json:"method"`
		Params struct {
			TargetID   string     `json:"targetId"`
			TargetInfo TargetInfo `json:"targetInfo"`
		} `json:"params"`
	}

	ignoredTypes := map[string]bool{
		"worker": true, "service_worker": true,
		"shared_worker": true, "background_page": true,
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// 连接断开确认应用状态
			fmt.Printf("与 %s 调试端口的连接断开，正在确认应用状态...\n", cfg.Name)
			time.Sleep(1500 * time.Millisecond)
			app := DetectApp(cfg)
			if !app.Running {
				fmt.Printf("检测到 %s 已退出，结束汉化监视。\n", cfg.Name)
				return
			}
			Watch(cfg, overlaySource, injectedSet, mu)
			return
		}

		var notif CDPNotification
		if err := json.Unmarshal(msg, &notif); err != nil {
			continue
		}

		// 监听目标销毁（如 Splash 页面关闭），及时清理状态
		if notif.Method == "Target.targetDestroyed" {
			targetID := notif.Params.TargetID
			if targetID != "" {
				mu.Lock()
				delete(injectedSet, targetID)
				delete(injectionPending, targetID)
				mu.Unlock()
			}
			continue
		}

		// 监听目标创建与变更
		if notif.Method == "Target.targetCreated" || notif.Method == "Target.targetInfoChanged" {
			info := notif.Params.TargetInfo
			if info.TargetID == "" || ignoredTypes[info.Type] {
				continue
			}

			t := CDPTarget{
				ID:                   info.TargetID,
				Type:                 info.Type,
				Title:                info.Title,
				URL:                  info.URL,
				WebSocketDebuggerURL: fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", cfg.Port, info.TargetID),
			}

			injectIfMatch(t, info.URL, "事件驱动")
		}
	}
}
