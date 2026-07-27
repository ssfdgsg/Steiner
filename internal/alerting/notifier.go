// webhook 通知器：异步队列 + 指数退避重试 + 多种消息模板。
// 投递永不阻塞产生事件的求值循环，队列满时丢弃并记录。
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/config"
)

// 投递结果，用于指标回调。
const (
	OutcomeOK      = "ok"
	OutcomeFailed  = "failed"  // 重试耗尽仍失败
	OutcomeDropped = "dropped" // 队列满被丢弃
)

// Notifier 管理全部 webhook 目标并异步投递事件。
type Notifier struct {
	targets  map[string]config.WebhookConfig
	order    []string // 保持配置顺序，便于测试与日志稳定
	queue    chan delivery
	client   *http.Client
	onResult func(target, outcome string) // 指标回调，可为 nil
}

type delivery struct {
	ev     Event
	target string
}

// NewNotifier 构造通知器。onResult 用于上报投递结果指标，可传 nil。
func NewNotifier(cfgs []config.WebhookConfig, onResult func(target, outcome string)) *Notifier {
	n := &Notifier{
		targets:  make(map[string]config.WebhookConfig, len(cfgs)),
		queue:    make(chan delivery, 256),
		client:   &http.Client{},
		onResult: onResult,
	}
	for _, c := range cfgs {
		n.targets[c.Name] = c
		n.order = append(n.order, c.Name)
	}
	return n
}

// Send 将事件投递到指定目标；targets 为空表示全部目标。非阻塞。
func (n *Notifier) Send(ev Event, targets []string) {
	if len(targets) == 0 {
		targets = n.order
	}
	for _, t := range targets {
		if _, ok := n.targets[t]; !ok {
			continue // 配置校验已保证引用合法，这里兜底跳过
		}
		select {
		case n.queue <- delivery{ev: ev, target: t}:
		default:
			slog.Warn("webhook 队列已满，事件被丢弃", "target", t, "type", ev.Type, "rule", ev.Rule)
			n.report(t, OutcomeDropped)
		}
	}
}

// Run 消费队列并投递，直到 ctx 取消。单 worker 串行发送即可满足告警量级。
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-n.queue:
			n.deliver(ctx, d)
		}
	}
}

// deliver 按目标配置投递一条事件，失败则指数退避重试。
func (n *Notifier) deliver(ctx context.Context, d delivery) {
	cfg := n.targets[d.target]
	body, err := renderTemplate(cfg.Template, d.ev)
	if err != nil {
		slog.Error("webhook 消息渲染失败", "target", d.target, "err", err)
		n.report(d.target, OutcomeFailed)
		return
	}
	backoff := cfg.RetryBackoff.D()
	for attempt := 0; ; attempt++ {
		err = n.post(ctx, cfg, body)
		if err == nil {
			n.report(d.target, OutcomeOK)
			return
		}
		if attempt >= cfg.Retries {
			slog.Error("webhook 投递失败，重试耗尽", "target", d.target, "attempts", attempt+1, "err", err)
			n.report(d.target, OutcomeFailed)
			return
		}
		select {
		case <-ctx.Done():
			n.report(d.target, OutcomeFailed)
			return
		case <-time.After(backoff):
			backoff *= 2
		}
	}
}

// post 单次 HTTP 投递。
func (n *Notifier) post(ctx context.Context, cfg config.WebhookConfig, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, cfg.Timeout.D())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("响应状态码 %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) report(target, outcome string) {
	if n.onResult != nil {
		n.onResult(target, outcome)
	}
}

// renderTemplate 按模板类型渲染消息体。
func renderTemplate(template string, ev Event) ([]byte, error) {
	switch template {
	case "generic":
		return json.Marshal(ev)
	case "dingtalk":
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"title": eventTitle(ev), "text": eventMarkdown(ev)},
		})
	case "feishu":
		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": eventText(ev)},
		})
	case "wecom":
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": eventMarkdown(ev)},
		})
	case "slack":
		return json.Marshal(map[string]string{"text": eventText(ev)})
	default:
		return nil, fmt.Errorf("未知消息模板 %q", template)
	}
}

// eventTitle 生成事件标题。
func eventTitle(ev Event) string {
	switch ev.Type {
	case TypeRollout:
		return "【金丝雀发布】" + ev.Summary
	case TypeAutoscale:
		if ev.Scale != nil {
			verb := "扩容"
			if ev.Scale.Direction == "down" {
				verb = "缩容"
			}
			return fmt.Sprintf("【%s建议】模型 %s: %d -> %d",
				verb, ev.Scale.Model, ev.Scale.CurrentReplicas, ev.Scale.DesiredReplicas)
		}
		return "【扩缩容建议】"
	default:
		state := "触发"
		if ev.Status == StatusResolved {
			state = "恢复"
		}
		return fmt.Sprintf("【%s】告警 %s %s（%s）", ev.Severity, ev.Rule, state, ev.Instance)
	}
}

// eventText 生成纯文本消息。
func eventText(ev Event) string {
	var b strings.Builder
	b.WriteString(eventTitle(ev))
	if ev.Summary != "" {
		b.WriteString("\n摘要: " + ev.Summary)
	}
	if ev.Expr != "" {
		b.WriteString("\n表达式: " + ev.Expr)
	}
	if !ev.StartsAt.IsZero() {
		b.WriteString("\n开始: " + ev.StartsAt.Format(time.RFC3339))
	}
	if ev.Status == StatusResolved && !ev.EndsAt.IsZero() {
		b.WriteString("\n恢复: " + ev.EndsAt.Format(time.RFC3339))
	}
	if len(ev.Vars) > 0 {
		b.WriteString("\n指标:")
		for _, k := range sortedKeys(ev.Vars) {
			b.WriteString(fmt.Sprintf(" %s=%.3g", k, ev.Vars[k]))
		}
	}
	return b.String()
}

// eventMarkdown 生成 markdown 消息（钉钉/企业微信）。
func eventMarkdown(ev Event) string {
	var b strings.Builder
	b.WriteString("### " + eventTitle(ev) + "\n")
	if ev.Summary != "" {
		b.WriteString("- 摘要: " + ev.Summary + "\n")
	}
	if ev.Expr != "" {
		b.WriteString("- 表达式: `" + ev.Expr + "`\n")
	}
	if !ev.StartsAt.IsZero() {
		b.WriteString("- 开始: " + ev.StartsAt.Format(time.RFC3339) + "\n")
	}
	if ev.Status == StatusResolved && !ev.EndsAt.IsZero() {
		b.WriteString("- 恢复: " + ev.EndsAt.Format(time.RFC3339) + "\n")
	}
	for _, k := range sortedKeys(ev.Vars) {
		b.WriteString(fmt.Sprintf("- %s: %.3g\n", k, ev.Vars[k]))
	}
	return b.String()
}
