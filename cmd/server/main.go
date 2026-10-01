// main.go free2api 命令行入口。
//
// 装配逻辑已抽到 internal/gateway（桌面程序 cmd/desktop 共用同一份），这里只留
// 参数解析 + 信号处理。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"free2api/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := gateway.Run(ctx, *cfgPath); err != nil {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
