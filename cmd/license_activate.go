package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iyear/tdl/license"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/cobra"
)

// remoteActivateResponse 与服务端 license.ActivateResponse 对应。
// 这里不复用那个类型：客户端只关心几个字段，独立定义可以在服务端演进时
// 保持老客户端继续工作。
type remoteActivateResponse struct {
	OK        bool       `json:"ok"`
	Code      string     `json:"code"`
	Message   string     `json:"message"`
	Device    string     `json:"device"`
	DaysLeft  int        `json:"days_left"`
	Forever   bool       `json:"forever"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// newLicenseActivate 是 `tdl license activate`：在命令行完成激活。
//
// 桌面版与 Docker 版没有激活页，这个命令是它们的入口。带 --server 时先向
// 授权服务登记（把密钥绑定到本机），再写本地状态；不带 --server 时只做本地
// 验签，适用于完全离线的部署。
func newLicenseActivate() *cobra.Command {
	var (
		key    string
		server string
		dir    string
	)

	cmd := &cobra.Command{
		Use:   "activate",
		Short: "激活本机（命令行入口，桌面/Docker 版使用）",
		Long: `激活本机。

两种模式：

  联网（推荐）  --server https://your-license-host
      先向授权服务登记，把密钥绑定到本机设备码。服务端会拒绝第二台设备使用
      同一把密钥，这是"一密钥一设备"唯一可靠的位置。

  离线         不带 --server
      只做本地验签。密钥里的设备码必须匹配本机，否则拒绝。适合无外网的部署，
      代价是换机必须重新签发（服务端无法放行）。

设备码可以用 ` + "`tdl license device`" + ` 查到。`,
		Example: "  tdl license activate --key P2NV18N5-222C8PKD-2TYRNSC3-THMCNGSF-2H5SPB6G\n" +
			"  tdl license activate --key P2NV... --server https://lic.example.com",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key = strings.TrimSpace(key)
			if key == "" {
				return errors.New("--key 必填")
			}
			if !license.Injected() {
				return errors.New("这个构建没有内置校验材料，无法激活；请使用发行版")
			}
			ver, err := license.BuildVerifier()
			if err != nil {
				return fmt.Errorf("校验材料无效: %w", err)
			}

			code, parts, err := license.LocalDeviceCode()
			if err != nil {
				return fmt.Errorf("读取本机标识失败: %w", err)
			}
			device := license.FormatDeviceCode(code)
			out := cmd.OutOrStdout()

			fmt.Fprintf(out, "本机设备码 %s\n", device)
			fmt.Fprintf(out, "  来源 %s\n", strings.Join(parts, " + "))

			if s := strings.TrimSpace(server); s != "" {
				fmt.Fprintf(out, "向授权服务登记 %s ...\n", s)
				resp, err := postActivate(cmd, s, "/v1/activate", key, device)
				if err != nil {
					return err
				}
				if !resp.OK {
					return fmt.Errorf("授权服务拒绝：%s（%s）", firstNonEmpty(resp.Message, resp.Code), resp.Code)
				}
				fmt.Fprintf(out, "  服务端已绑定到 %s\n", resp.Device)
				if resp.Forever {
					fmt.Fprintln(out, "  有效期 永久")
				} else if resp.ExpiresAt != nil {
					fmt.Fprintf(out, "  有效期 至 %s（剩 %d 天）\n", resp.ExpiresAt.Format("2006-01-02"), resp.DaysLeft)
				}
			}

			if strings.TrimSpace(dir) == "" {
				dir = consts.DataDir + "/license"
			}
			mgr := license.NewManager(dir, ver, code)
			st, err := mgr.Activate(key, time.Now())
			if err != nil {
				return fmt.Errorf("本地激活失败: %w", err)
			}

			// Remember where this activation came from so later starts can be
			// re-checked (and revoked) without the user retyping the address.
			if s := strings.TrimSpace(server); s != "" {
				if err := mgr.SetServer(s); err != nil {
					return fmt.Errorf("记录授权服务地址失败: %w", err)
				}
				if err := mgr.MarkChecked(time.Now()); err != nil {
					return fmt.Errorf("记录校验时间失败: %w", err)
				}
			}

			fmt.Fprintf(out, "\n激活成功\n")
			if st.Valid {
				fmt.Fprintf(out, "  状态 有效\n")
			} else {
				fmt.Fprintf(out, "  状态 %s\n", st.Reason)
			}
			if st.Activated {
				fmt.Fprintf(out, "  密钥 %s\n", license.KeyFingerprint(key))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&key, "key", "", "授权密钥")
	cmd.Flags().StringVar(&server, "server", "", "授权服务地址（可选；不给则只做本地验签）")
	cmd.Flags().StringVar(&dir, "dir", "", "本地状态目录（默认数据目录下的 license/）")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}

// postActivate 向授权服务发一次 activate/verify。
func postActivate(cmd *cobra.Command, base, path, key, device string) (remoteActivateResponse, error) {
	body, _ := json.Marshal(map[string]string{"key": key, "device": device})
	url := strings.TrimRight(base, "/") + path

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return remoteActivateResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(req)
	if err != nil {
		return remoteActivateResponse{}, fmt.Errorf("连接授权服务失败: %w", err)
	}
	defer httpResp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64<<10))
	var resp remoteActivateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("授权服务返回非 JSON（HTTP %d）: %s", httpResp.StatusCode, truncate(string(raw), 200))
	}
	return resp, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// decodeRemote 读取并解析授权服务的一次响应。
func decodeRemote(resp *http.Response) (remoteActivateResponse, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out remoteActivateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("授权服务返回非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return out, nil
}
