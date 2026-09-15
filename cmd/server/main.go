// main.go workbuddy2api 服务端入口（Console 子系统）：加载配置、装配、起 HTTP、等信号。
//
// 装配逻辑全在 internal/app（与 cmd/desktop 共用）；本文件只保留"服务端特有的
// 生命周期"——命令行 -config 参数、信号驱动的优雅停机。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/linguo2625469/workbuddy2api-panel/internal/app"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := app.LoadOrInitConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	inst, err := app.Build(cfg, *cfgPath)
	if err != nil {
		log.Fatalf("build: %v", err)
	}
	defer inst.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		inst.Shutdown() // 信号触发：先落盘再做优雅停机
	}()

	if err := inst.Serve(); err != nil {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
