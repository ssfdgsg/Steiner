// M16 回归测试：模型名改写 / PD 转发经 map 重序列化丢失数值精度。
//
// 缺陷：parseRequestDocument 用 json.Unmarshal 解析，文档内数字统一转 float64，
// 重序列化时 >2^53 的大整数（max_tokens、毫秒时间戳、seed 等）被舍入。
// 修复：解析入口改用 json.Decoder + UseNumber，文档内数值以 json.Number 保留原文，
// 重序列化时原样输出。以下"精度保留"断言在修复前（float64 舍入）失败、修复后通过。
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/scheduler"
)

// 超过 float64 精确整数范围（2^53）的样例字面量：float64 均无法精确表示。
const (
	bigMaxTokens  = "9007199254740993"     // 2^53+1
	bigTimestamp  = "17000000000001234"    // >2^53 的毫秒时间戳
	bigSeed       = "-9223372036854775807" // int64 min+1
	bigE18PlusOne = "1000000000000000001"  // 1e18+1
)

// decodeNumbers 用 UseNumber 解码，数值以 json.Number 保留原文（与 parse 修复后一致）。
func decodeNumbers(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]interface{}
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return doc
}

func numStr(t *testing.T, v interface{}) string {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("数值字段不是 json.Number: %T %v", v, v)
	}
	return n.String()
}

func msgTimestamp(t *testing.T, doc map[string]interface{}) string {
	t.Helper()
	msgs, ok := doc["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages 结构不符: %v", doc["messages"])
	}
	msg, ok := msgs[0].(map[string]interface{})
	if !ok {
		t.Fatalf("message 结构不符: %v", msgs[0])
	}
	return numStr(t, msg["timestamp"])
}

// TestRewriteRoundTripPreservesBigInts 模拟 proxy.go 模型名改写路径：
// parse → 改写 model → 整体重序列化，大整数应原样保留。修复前 max_tokens
// 9007199254740993 经 float64 舍入为 9007199254740992，断言失败（缺陷复现）。
func TestRewriteRoundTripPreservesBigInts(t *testing.T) {
	body := []byte(`{"model":"m1","max_tokens":9007199254740993,"priority":2.5,
		"messages":[{"role":"user","content":"hi","timestamp":17000000000001234}]}`)
	doc := parseRequestDocument(body)
	if doc == nil {
		t.Fatal("解析失败")
	}
	// 与 proxy.go:272-274 完全一致：改写 model 后 json.Marshal(doc) 替换 body。
	doc["model"] = "rewritten"
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	got := decodeNumbers(t, out)
	if got["model"] != "rewritten" {
		t.Fatalf("model 改写未生效: %v", got["model"])
	}
	if mt := numStr(t, got["max_tokens"]); mt != bigMaxTokens {
		t.Fatalf("max_tokens 精度丢失: got=%s want=%s", mt, bigMaxTokens)
	}
	if ts := msgTimestamp(t, got); ts != bigTimestamp {
		t.Fatalf("messages[0].timestamp 精度丢失: got=%s want=%s", ts, bigTimestamp)
	}
	if p := numStr(t, got["priority"]); p != "2.5" {
		t.Fatalf("普通浮点被改写: got=%s", p)
	}
}

// TestPDRoundTripPreservesBigInts 模拟 pdproxy.go vllm 两段式转发路径：
// 浅拷贝文档 → 注入 max_tokens=1 / stream=false → 重序列化，
// seed / timestamp 等大整数应原样保留。
func TestPDRoundTripPreservesBigInts(t *testing.T) {
	body := []byte(`{"model":"m1","seed":-9223372036854775807,"max_tokens":9007199254740993,
		"messages":[{"role":"user","content":"hi","timestamp":17000000000001234}]}`)
	doc := parseRequestDocument(body)
	if doc == nil {
		t.Fatal("解析失败")
	}

	// 与 pdproxy.go cloneDoc + pdVLLM 首段改写一致（行内浅拷贝，避免依赖并行改动中的 cloneDoc）。
	prefillDoc := map[string]interface{}{}
	for k, v := range doc {
		prefillDoc[k] = v
	}
	prefillDoc["max_tokens"] = 1
	prefillDoc["stream"] = false
	prefillDoc["kv_transfer_params"] = map[string]interface{}{"do_remote_decode": true}
	out, err := json.Marshal(prefillDoc)
	if err != nil {
		t.Fatal(err)
	}

	got := decodeNumbers(t, out)
	if mt := numStr(t, got["max_tokens"]); mt != "1" {
		t.Fatalf("prefill max_tokens 应被改写为 1: got=%s", mt)
	}
	if s := numStr(t, got["seed"]); s != bigSeed {
		t.Fatalf("seed 精度丢失: got=%s want=%s", s, bigSeed)
	}
	if ts := msgTimestamp(t, got); ts != bigTimestamp {
		t.Fatalf("messages[0].timestamp 精度丢失: got=%s want=%s", ts, bigTimestamp)
	}
}

// TestRewriteRoundTripPreservesE18 覆盖 1e18 数量级样例。
func TestRewriteRoundTripPreservesE18(t *testing.T) {
	body := []byte(`{"model":"m1","request_id":1000000000000000001}`)
	doc := parseRequestDocument(body)
	if doc == nil {
		t.Fatal("解析失败")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeNumbers(t, out)
	if id := numStr(t, got["request_id"]); id != bigE18PlusOne {
		t.Fatalf("request_id 精度丢失: got=%s want=%s", id, bigE18PlusOne)
	}
}

// TestPriorityAcceptsNumberForms toFloat64 兼容性：priority 字段无论来自
// UseNumber 解码（json.Number）还是原生解码（float64）都应落到调度请求。
func TestPriorityAcceptsNumberForms(t *testing.T) {
	// json.Number 形态（修复后 parseRequestDocument 的产出）。
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req, _ := parseRequest(r, []byte(`{"model":"m1","priority":2.5}`))
	if req.Priority != 2.5 {
		t.Fatalf("json.Number 形态 priority 解析失败: got=%v", req.Priority)
	}
	// float64 形态（旧解码路径构造的 doc 直调 populateRequestBase，须保持兼容）。
	doc := map[string]interface{}{"model": "m1", "priority": 3.75}
	req2 := &scheduler.Request{}
	populateRequestBase(req2, doc)
	if req2.Priority != 3.75 || req2.Model != "m1" {
		t.Fatalf("float64 形态 priority 解析失败: %+v", req2)
	}
}

// TestParseRequestDocumentRejectsMalformed 解析语义守卫：非对象/尾随垃圾
// 仍返回 nil，与修复前 json.Unmarshal 行为一致。
func TestParseRequestDocumentRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		``, `not json`, `[1,2]`, `{"a":1}{"b":2}`, `{"a":1} x`, `null`,
	} {
		if doc := parseRequestDocument([]byte(raw)); doc != nil {
			t.Fatalf("非法输入应返回 nil: %q → %v", raw, doc)
		}
	}
	if doc := parseRequestDocument([]byte(`{"a":1} `)); doc == nil {
		t.Fatal("尾随空白应视为合法 JSON 对象")
	}
}

// TestRewriteModelWireBodyPreservesBigInts 端到端：走 Handler 完整链路
// （读体 → parseRequestDocument → 模型名改写 → 重新 Marshal → 上游转发），
// 断言上游收到的原始字节里大整数原样保留。这是 M16 的直接触发路径。
func TestRewriteModelWireBodyPreservesBigInts(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer upstream.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{{ID: "b1", URL: upstream.URL, Engine: "vllm"}},
		Models: []config.ModelRoute{{
			Name: "m1", Backends: []string{"b1"}, RewriteModel: "deployed-m1",
		}},
	})
	body := []byte(`{"model":"m1","max_tokens":9007199254740993,"priority":2.5,
		"messages":[{"role":"user","content":"hi","timestamp":17000000000001234}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("转发失败: code=%d body=%s", rec.Code, rec.Body.String())
	}

	got := decodeNumbers(t, gotBody)
	if got["model"] != "deployed-m1" {
		t.Fatalf("上游应收到改写后的模型名: %v", got["model"])
	}
	if mt := numStr(t, got["max_tokens"]); mt != bigMaxTokens {
		t.Fatalf("上游 max_tokens 精度丢失: got=%s want=%s", mt, bigMaxTokens)
	}
	if ts := msgTimestamp(t, got); ts != bigTimestamp {
		t.Fatalf("上游 messages[0].timestamp 精度丢失: got=%s want=%s", ts, bigTimestamp)
	}
	if p := numStr(t, got["priority"]); p != "2.5" {
		t.Fatalf("上游 priority 被改写: got=%s", p)
	}
}

// TestPDWireBodiesPreserveBigInts 端到端：vllm 两段式 PD 转发完整链路，
// prefill 与 decode 收到的原始字节里大整数都应原样保留。
func TestPDWireBodiesPreserveBigInts(t *testing.T) {
	var prefillBody, decodeBody []byte
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefillBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// kv_transfer_params 用字符串 block id，避免把 out-of-scope 的
		// pdproxy 响应重序列化路径（float64）混入本测试断言。
		_, _ = io.WriteString(w, `{"kv_transfer_params":{"remote_host":"127.0.0.1",
			"remote_port":50051,"remote_block_ids":["block-1"]}}`)
	}))
	defer prefillSrv.Close()
	decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"dec-ok","choices":[{"index":0,
			"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer decodeSrv.Close()

	st := newStack(t, &config.Config{
		Backends: []config.BackendConfig{
			{ID: "p1", URL: prefillSrv.URL, Engine: "vllm"},
			{ID: "d1", URL: decodeSrv.URL, Engine: "vllm"},
		},
		PDGroups: []config.PDGroupConfig{{
			Name: "g1", Prefill: []string{"p1"}, Decode: []string{"d1"},
		}},
		Models: []config.ModelRoute{{Name: "m1", PDGroup: "g1"}},
	})

	body := []byte(`{"model":"m1","seed":-9223372036854775807,"max_tokens":9007199254740993,
		"messages":[{"role":"user","content":"hi","timestamp":17000000000001234}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PD 转发失败: code=%d body=%s", rec.Code, rec.Body.String())
	}

	pb := decodeNumbers(t, prefillBody)
	if mt := numStr(t, pb["max_tokens"]); mt != "1" {
		t.Fatalf("prefill max_tokens 应被改写为 1: got=%s", mt)
	}
	if s := numStr(t, pb["seed"]); s != bigSeed {
		t.Fatalf("prefill seed 精度丢失: got=%s want=%s", s, bigSeed)
	}
	if ts := msgTimestamp(t, pb); ts != bigTimestamp {
		t.Fatalf("prefill messages[0].timestamp 精度丢失: got=%s want=%s", ts, bigTimestamp)
	}

	db := decodeNumbers(t, decodeBody)
	if mt := numStr(t, db["max_tokens"]); mt != bigMaxTokens {
		t.Fatalf("decode max_tokens 精度丢失: got=%s want=%s", mt, bigMaxTokens)
	}
	if s := numStr(t, db["seed"]); s != bigSeed {
		t.Fatalf("decode seed 精度丢失: got=%s want=%s", s, bigSeed)
	}
	if ts := msgTimestamp(t, db); ts != bigTimestamp {
		t.Fatalf("decode messages[0].timestamp 精度丢失: got=%s want=%s", ts, bigTimestamp)
	}
	ktp, ok := db["kv_transfer_params"].(map[string]interface{})
	if !ok {
		t.Fatalf("decode 应携带 kv_transfer_params: %v", db)
	}
	if host, _ := ktp["remote_host"].(string); host != "127.0.0.1" {
		t.Fatalf("decode kv_transfer_params.remote_host 不符: %v", ktp)
	}
}
