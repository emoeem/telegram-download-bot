package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/iyear/tdl/license"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/cobra"
)

// 每条业务命令执行前都要过的授权门。
//
// 为什么放在 PersistentPreRunE 而不是各命令自己检查：漏一个命令就是漏一个
// 免费入口，而"新增命令时记得加检查"这件事一定会忘。放在根上，新命令自动被覆盖。

// cliLicenceExempt 列出不需要授权的命令。
//
// license 是发行方工具（本身就要能跑）；version 让客户能确认版本；
// help/completion 是 shell 基础设施。除此之外一律要授权。
var cliLicenceExempt = map[string]bool{
	"license":    true,
	"version":    true,
	"help":       true,
	"completion": true,
}

// cliLicenceDir 是本地激活状态的目录，与 Web 激活页共用同一份状态。
func cliLicenceDir() string {
	return filepath.Join(consts.DataDir, "license")
}

// enforceCLILicence 检查本机授权，未通过则直接拒绝执行命令。
//
// 三种情况直接放行：
//   - 这个构建没有内置校验材料（源码构建，开发者不该被自己的授权门挡住）；
//   - 命令本身是发行方/版本/帮助类；
//   - 用户显式传了 --no-license（自建部署的逃生门，会在日志里留下警告）。
func enforceCLILicence(cmd *cobra.Command) error {
	if !license.Injected() {
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		if cliLicenceExempt[c.Name()] {
			return nil
		}
	}
	// --no-license 只对声明了它的命令生效。
	if f := cmd.Flags().Lookup("no-license"); f != nil && f.Value.String() == "true" {
		slog.Warn("licence check skipped by --no-license")
		return nil
	}

	ver, err := license.BuildVerifier()
	if err != nil {
		return fmt.Errorf("授权校验材料无效: %w", err)
	}
	code, _, err := license.LocalDeviceCode()
	if err != nil {
		return fmt.Errorf("读取本机标识失败: %w", err)
	}

	mgr := license.NewManager(cliLicenceDir(), ver, code)
	now := time.Now()

	// 优先联网核对：本地状态可能是几天前写的，而这期间密钥可能已被撤销。
	if mgr.NeedsOnlineCheck(now, licenceCheckInterval) {
		if err := refreshLicenceOnline(mgr, now); err != nil {
			slog.Warn("licence online check failed; falling back to local state",
				slog.String("error", err.Error()))
		}
	}

	if err := mgr.Gate(now); err != nil {
		return licenceBlockedError(mgr, code, err)
	}
	return nil
}

// licenceCheckInterval 是两次在线校验之间允许的最长间隔。
//
// 它就是"客户被撤销后还能用多久"的上限：12 小时意味着当天内收敛。
// 调小会让弱网客户的启动变慢，调大会拉长撤销延迟。
const licenceCheckInterval = 12 * time.Hour

// refreshLicenceOnline 向授权服务核对一次本机授权。
//
// 服务端明确说 revoked/expired 时清除本地激活并把错误往回传——这是撤销真正
// 生效的地方。网络错误不清除：客户在飞机上也要能用已买的授权。
func refreshLicenceOnline(mgr *license.Manager, now time.Time) error {
	server := mgr.Server()
	if server == "" {
		return nil
	}

	resp, err := postActivateRaw(server, "/v1/verify", mgr.CurrentKey(), mgr.ThisDevice())
	if err != nil {
		return err
	}
	switch resp.Code {
	case "ok":
		return mgr.MarkChecked(now)
	case "revoked", "expired", "unknown_key", "bound_elsewhere":
		// 服务端给出了确定的否定结论，本地必须跟着失效。
		_ = mgr.Clear()
		return fmt.Errorf("授权已失效（%s）：%s", resp.Code, resp.Message)
	default:
		return fmt.Errorf("授权服务返回 %s: %s", resp.Code, resp.Message)
	}
}

// postActivateRaw 是一个不依赖 cobra 的 POST 封装，供启动期校验使用。
func postActivateRaw(base, path, key, device string) (remoteActivateResponse, error) {
	body := fmt.Sprintf(`{"key":%q,"device":%q}`, key, device)
	url := strings.TrimRight(base, "/") + path

	client := &http.Client{Timeout: 12 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return remoteActivateResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return remoteActivateResponse{}, err
	}
	defer resp.Body.Close()

	return decodeRemote(resp)
}

// licenceBlockedError 把门的拒绝翻译成用户能照着做的一段话。
//
// 只说"未授权"会让客户来问"那怎么办"，所以每条分支都给下一步动作。
func licenceBlockedError(mgr *license.Manager, code uint64, cause error) error {
	device := license.FormatDeviceCode(code)
	switch {
	case license.IsNotActivated(cause):
		return fmt.Errorf(
			"本机尚未授权。\n"+
				"  设备码 %s\n"+
				"  激活    tdl license activate --key <密钥>\n"+
				"  购买/续期请把上面的设备码发给发行方", device)
	case errors.Is(cause, license.ErrExpired):
		st := mgr.Status(time.Now())
		return fmt.Errorf(
			"授权已过期（%s）。\n"+
				"  设备码 %s\n"+
				"  续期    tdl license activate --key <新密钥>", st.ExpiresAt, device)
	case errors.Is(cause, license.ErrDevice):
		return fmt.Errorf(
			"这把密钥不适用于本机。\n"+
				"  本机设备码 %s\n"+
				"  请把该设备码发给发行方换取对应密钥", device)
	default:
		return fmt.Errorf("授权校验未通过: %w", cause)
	}
}
