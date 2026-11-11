package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iyear/tdl/license"
	"github.com/spf13/cobra"
)

// newLicenseServer 是 `tdl license server`：在线激活与撤销服务。
//
// 它补齐离线签名做不到的三件事：一密钥一设备、远程撤销、换机放行。
// 服务本身只监听回环地址——对外暴露交给 Cloudflare Tunnel，见 --tunnel 的说明。
func newLicenseServer() *cobra.Command {
	var (
		dbPath     string
		addr       string
		adminToken string
		tunnel     bool
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "启动在线授权服务（激活 / 心跳 / 撤销）",
		Long: `启动在线授权服务。

为什么需要它：离线签名只能证明"密钥是发行方签的"，证明不了"这把密钥只用在
一台设备上"。客户端文件可以被复制，签名无法阻止复制。把绑定关系放到服务端，
复制就失去了意义——第二台设备拿着同一把密钥来激活，服务端会拒绝。

对外暴露（三条路，任选其一）：
  1. Cloudflare Tunnel（推荐，不需要公网 IP、不需要开端口、免费）
       cloudflared tunnel --url http://127.0.0.1:8964
     拿到一个 https://xxx.trycloudflare.com 临时域名；要固定域名就用
     cloudflared tunnel create + 绑定自己的域名。
  2. 反向代理（nginx/caddy）到你自己的域名。
  3. 内网直连（客户与服务器同网段时）。

运营接口需要 --admin-token；不传则运营接口全部关闭，只保留激活/校验。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbPath == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				dbPath = filepath.Join(home, ".tdl-license", "registry.json")
			}

			reg, err := license.OpenRegistry(dbPath)
			if err != nil {
				return err
			}

			srv := license.NewServer(reg, adminToken, slog.Default())

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("监听 %s 失败: %w", addr, err)
			}

			httpSrv := &http.Server{
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				// 这个服务处理的是几十字节的 JSON，不需要长连接大超时；
				// 短超时能尽早回收被扫描器占住的连接。
				ReadTimeout:  15 * time.Second,
				WriteTimeout: 15 * time.Second,
				IdleTimeout:  60 * time.Second,
			}

			fmt.Printf("授权服务已启动\n")
			fmt.Printf("  监听    http://%s\n", ln.Addr().String())
			fmt.Printf("  账本    %s（%d 把密钥）\n", dbPath, reg.Count())
			if adminToken == "" {
				fmt.Printf("  运营接口 已关闭（未设置 --admin-token）\n")
			} else {
				fmt.Printf("  运营接口 已开启（Bearer 认证）\n")
			}
			fmt.Printf("\n客户端接入：\n")
			fmt.Printf("  tdl license activate --key <密钥> --server http://%s\n", ln.Addr().String())
			fmt.Printf("\n对外暴露（Cloudflare Tunnel）：\n")
			fmt.Printf("  cloudflared tunnel --url http://%s\n", ln.Addr().String())
			if tunnel {
				fmt.Printf("\n注意：本机未自动拉起 cloudflared。装好后执行上面那条命令即可拿到公网地址。\n")
			}

			// 收到信号后优雅退出，把账本刷干净（每次写都是即时落盘，
			// 这里只是让正在处理的请求结束）。
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			errCh := make(chan error, 1)
			go func() { errCh <- httpSrv.Serve(ln) }()

			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(shutdownCtx)
				fmt.Println("\n授权服务已停止")
				return nil
			case err := <-errCh:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "", "账本文件路径（默认 ~/.tdl-license/registry.json）")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8964", "监听地址；默认只监听回环，对外由 cloudflared 转发")
	cmd.Flags().StringVar(&adminToken, "admin-token", "", "运营接口令牌（撤销/解绑/登记/列表）；留空则关闭运营接口")
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "启动时提示 Cloudflare Tunnel 接法")
	return cmd
}
