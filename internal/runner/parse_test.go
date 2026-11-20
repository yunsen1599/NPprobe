package runner

import (
	"encoding/json"
	"testing"
)

// 面板下发的代理配置经过 JSON（数字是 float64、嵌套是 map[string]any），mihomo 要能直接解析
func TestMihomoParsesPanelJSON(t *testing.T) {
	cases := map[string]string{
		"vless-reality": `{"name":"a","type":"vless","server":"a.example.com","port":443,"uuid":"11111111-2222-4333-8444-555555555555","network":"tcp","tls":true,"flow":"xtls-rprx-vision","servername":"www.apple.com","client-fingerprint":"chrome","reality-opts":{"public-key":"Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw","short-id":"ab"}}`,
		"vless-xhttp":   `{"name":"b","type":"vless","server":"b.example.com","port":443,"uuid":"11111111-2222-4333-8444-555555555555","network":"xhttp","tls":true,"servername":"b.example.com","xhttp-opts":{"path":"/x","mode":"auto"}}`,
		"vmess-ws":      `{"name":"c","type":"vmess","server":"c.example.com","port":443,"uuid":"11111111-2222-4333-8444-555555555555","alterId":0,"cipher":"auto","network":"ws","tls":true,"ws-opts":{"path":"/ws","headers":{"Host":"c.example.com"}}}`,
		"trojan":        `{"name":"d","type":"trojan","server":"d.example.com","port":443,"password":"p","sni":"d.example.com","skip-cert-verify":true}`,
		"ss2022":        `{"name":"e","type":"ss","server":"e.example.com","port":8388,"cipher":"2022-blake3-aes-128-gcm","password":"AAAAAAAAAAAAAAAAAAAAAA==:AAAAAAAAAAAAAAAAAAAAAA=="}`,
		"hysteria2":     `{"name":"f","type":"hysteria2","server":"f.example.com","port":443,"ports":"20000-30000","password":"p","obfs":"salamander","obfs-password":"o","sni":"f.example.com","skip-cert-verify":true}`,
		"tuic":          `{"name":"g","type":"tuic","server":"g.example.com","port":443,"uuid":"11111111-2222-4333-8444-555555555555","password":"p","sni":"g.example.com","alpn":["h3"],"congestion-controller":"bbr"}`,
		"anytls":        `{"name":"h","type":"anytls","server":"h.example.com","port":443,"password":"p","sni":"h.example.com"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatal(err)
			}
			_, closeFn, err := MihomoFactory(m)
			if err != nil {
				t.Fatalf("mihomo rejected panel config: %v", err)
			}
			closeFn()
		})
	}
}
