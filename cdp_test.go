package main

import "testing"

func TestIsTargetPageIDEWorkbenches(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "main workbench", url: "file:///Applications/Antigravity%20IDE.app/Contents/Resources/app/out/vs/code/electron-browser/workbench/workbench.html", want: true},
		{name: "main workbench with query", url: "vscode-file://vscode-app/Contents/Resources/app/out/vs/code/electron-browser/workbench/workbench.html?folder=/tmp/project", want: true},
		{name: "jetski workbench", url: "file:///tmp/workbench-jetski-agent.html", want: true},
		{name: "unrelated page", url: "devtools://devtools/bundled/inspector.html", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTargetPage(tt.url, AppIDE.Name); got != tt.want {
				t.Fatalf("isTargetPage(%q, %q) = %v, want %v", tt.url, AppIDE.Name, got, tt.want)
			}
		})
	}
}

func TestIsTargetPageAppNormal(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "local internal service url", url: "https://127.0.0.1:49689/c/34edf1e5-b701-4d5a-9db0-a4af67ecd229", want: true},
		{name: "localhost url", url: "http://localhost:3000/app", want: true},
		{name: "splash data url", url: "data:text/html;base64,PGh0bWw+...", want: true},
		{name: "devtools inspector", url: "devtools://devtools/bundled/inspector.html", want: false},
		{name: "external web url", url: "https://google.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTargetPage(tt.url, AppNormal.Name); got != tt.want {
				t.Fatalf("isTargetPage(%q, %q) = %v, want %v", tt.url, AppNormal.Name, got, tt.want)
			}
		})
	}
}

func TestCDPResponseError(t *testing.T) {
	if err := cdpResponseError(map[string]interface{}{
		"error": map[string]interface{}{"message": "method not found"},
	}, "Page.enable"); err == nil {
		t.Fatal("expected CDP protocol error")
	}

	if err := cdpResponseError(map[string]interface{}{
		"result": map[string]interface{}{
			"exceptionDetails": map[string]interface{}{
				"text": "Uncaught SyntaxError",
			},
		},
	}, "Runtime.evaluate"); err == nil {
		t.Fatal("expected Runtime.evaluate exception")
	}

	if err := cdpResponseError(map[string]interface{}{
		"result": map[string]interface{}{"result": map[string]interface{}{"value": true}},
	}, "Runtime.evaluate"); err != nil {
		t.Fatalf("unexpected response error: %v", err)
	}
}
