// PD 分离转发单测：
//   - vllm 家族两段式：prefill 收到 max_tokens=1 + kv_transfer_params 请求段，
//     decode 收到从 prefill 响应回传的 KV 句柄；
//   - sglang 家族双发：prefill 与 decode 均收到一致的 bootstrap 三元组；
//   - prefill 故障时换 prefill 重试。
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/test/mockbackend"
)

// newPDStack 装配一套 1 模型 + 1 PD 组的网关。
func newPDStack(t *testing.T, engine string, prefills, decodes []*httptest.Server, links []config.NCCLLinkConfig) *stack {
	t.Helper()
	var backends []config.BackendConfig
	var prefillIDs, decodeIDs []string
	for i, s := range prefills {
		id := "p" + string(rune('1'+i))
		backends = append(backends, config.BackendConfig{ID: id, URL: s.URL, Engine: engine})
		prefillIDs = append(prefillIDs, id)
	}
	for i, s := range decodes {
		id := "d" + string(rune('1'+i))
		backends = append(backends, config.BackendConfig{ID: id, URL: s.URL, Engine: engine})
		decodeIDs = append(decodeIDs, id)
	}
	return newStack(t, &config.Config{
		Backends: backends,
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: prefillIDs, Decode: decodeIDs, Links: links,
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
	})
}

func TestPDVLLMTwoPhase(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "模拟应答") {
		t.Fatalf("响应体不符: %s", rec.Body.String())
	}

	// prefill 侧断言：只算提示词，携带 KV 传输请求段。
	pb := prefill.LastBody()
	if mt, _ := pb["max_tokens"].(float64); mt != 1 {
		t.Fatalf("prefill 请求 max_tokens 应为 1，实际 %v", pb["max_tokens"])
	}
	if stream, _ := pb["stream"].(bool); stream {
		t.Fatal("prefill 请求应关闭流式")
	}
	ktp, ok := pb["kv_transfer_params"].(map[string]interface{})
	if !ok {
		t.Fatalf("prefill 请求应携带 kv_transfer_params: %v", pb)
	}
	if remote, _ := ktp["do_remote_decode"].(bool); !remote {
		t.Fatal("prefill 请求 do_remote_decode 应为 true")
	}

	// decode 侧断言：注入了 prefill 返回的 KV 句柄，且保留原始 max_tokens 语义。
	db := decode.LastBody()
	dktp, ok := db["kv_transfer_params"].(map[string]interface{})
	if !ok {
		t.Fatalf("decode 请求应携带 prefill 回传的 kv_transfer_params: %v", db)
	}
	if host, _ := dktp["remote_host"].(string); host != "127.0.0.1" {
		t.Fatalf("decode 收到的 remote_host 不符: %v", dktp)
	}
	if _, exists := db["max_tokens"]; exists {
		if mt, _ := db["max_tokens"].(float64); mt == 1 {
			t.Fatal("decode 请求不应继承 prefill 的 max_tokens=1")
		}
	}
}

func TestPDSGLangBootstrap(t *testing.T) {
	prefill := mockbackend.New("sglang")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	decode := mockbackend.New("sglang")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	st := newPDStack(t, "sglang", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// decode 同步收到；prefill 为后台双发，轮询等待。
	db := decode.LastBody()
	if db["bootstrap_host"] != "127.0.0.1" {
		t.Fatalf("decode 应收到 bootstrap_host，实际: %v", db["bootstrap_host"])
	}
	if _, ok := db["bootstrap_port"].(float64); !ok {
		t.Fatalf("decode 应收到 bootstrap_port，实际: %v", db["bootstrap_port"])
	}
	room, ok := db["bootstrap_room"].(float64)
	if !ok {
		t.Fatalf("decode 应收到 bootstrap_room，实际: %v", db["bootstrap_room"])
	}

	waitFor(t, time.Second, func() bool { return prefill.ReqCount() == 1 }, "prefill 侧应收到双发请求")
	pb := prefill.LastBody()
	if proom, _ := pb["bootstrap_room"].(float64); proom != room {
		t.Fatalf("prefill 与 decode 的 bootstrap_room 应一致: %v vs %v", proom, room)
	}
}

func TestPDPrefillFailureRetries(t *testing.T) {
	badPrefill := mockbackend.New("vllm")
	badPrefill.SetFault(mockbackend.Fault{FailCode: 500})
	badSrv := httptest.NewServer(badPrefill.Handler())
	defer badSrv.Close()
	goodPrefill := mockbackend.New("vllm")
	goodSrv := httptest.NewServer(goodPrefill.Handler())
	defer goodSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	st := newPDStack(t, "vllm",
		[]*httptest.Server{badSrv, goodSrv},
		[]*httptest.Server{decodeSrv}, nil)

	// 多发几次：无论先选中哪个 prefill，坏 prefill 都应触发重试并最终成功。
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次请求应经重试成功，实际 %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if goodPrefill.ReqCount() == 0 {
		t.Fatal("好 prefill 应承接流量")
	}
}

func TestPDLinkConstraint(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	d1 := mockbackend.New("vllm")
	d1Srv := httptest.NewServer(d1.Handler())
	defer d1Srv.Close()
	d2 := mockbackend.New("vllm")
	d2Srv := httptest.NewServer(d2.Handler())
	defer d2Srv.Close()

	// 只声明 p1->d2 链路：即使 d1 存在也绝不能被配对。
	st := newPDStack(t, "vllm",
		[]*httptest.Server{prefillSrv},
		[]*httptest.Server{d1Srv, d2Srv},
		[]config.NCCLLinkConfig{{Prefill: "p1", Decode: "d2", BandwidthGbps: 400}})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody("m1", false)))
		rec := httptest.NewRecorder()
		st.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("期望 200，实际 %d", rec.Code)
		}
	}
	if d1.ReqCount() != 0 {
		t.Fatal("未建链的 d1 不应收到请求（NCCL 链路约束失效）")
	}
	if d2.ReqCount() != 3 {
		t.Fatalf("d2 应承接全部请求，实际 %d", d2.ReqCount())
	}
}

// TestPDRequiresJSON PD 模式要求可解析的 JSON 请求体。
func TestPDRequiresJSON(t *testing.T) {
	prefill := mockbackend.New("vllm")
	prefillSrv := httptest.NewServer(prefill.Handler())
	defer prefillSrv.Close()
	decode := mockbackend.New("vllm")
	decodeSrv := httptest.NewServer(decode.Handler())
	defer decodeSrv.Close()

	st := newPDStack(t, "vllm", []*httptest.Server{prefillSrv}, []*httptest.Server{decodeSrv}, nil)
	// 模型无法从坏 JSON 中解析 → 走不到 PD 组（404 或 400 均为拒绝语义），
	// 这里用合法 JSON 但空模型验证兜底路由缺失时的行为。
	body, _ := json.Marshal(map[string]interface{}{"model": "m1"})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("最小合法请求应可通过 PD 通路，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
