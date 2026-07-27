// mock 后端独立进程入口，供本地冒烟脚本使用。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"ai-gateway/test/mockbackend"
)

func main() {
	engine := flag.String("type", "vllm", "引擎类型: vllm | vllm_omni | sglang | sglang_omni")
	port := flag.Int("port", 8001, "监听端口")
	flag.Parse()

	m := mockbackend.New(*engine)
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("mock 后端启动 engine=%s addr=%s", *engine, addr)
	if err := http.ListenAndServe(addr, m.Handler()); err != nil {
		log.Fatal(err)
	}
}
