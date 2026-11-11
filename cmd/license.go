package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iyear/tdl/license"
	"github.com/spf13/cobra"
)

// NewLicense is the vendor-only side of the licence system: generating the
// master secret, deriving the build-time verifier material from it, and issuing
// per-customer keys.
//
// These commands ship in the binary because the vendor uses the same executable
// to build and to sell. They are harmless without the master key file, which is
// what actually gates issuance.
func NewLicense() *cobra.Command {
	root := &cobra.Command{
		Use:   "license",
		Short: "签发与校验商业授权密钥（发行方使用）",
		Long: "签发与校验商业授权密钥。\n\n" +
			"主密钥（TDLSK1-…）是唯一需要保密的东西：它同时派生短密钥的校验串和强密钥的\n" +
			"签名私钥。把它保存在你自己的电脑上，不要放进仓库、不要发给客户。",
		GroupID: groupTools.ID,
	}
	root.AddCommand(newLicenseKeygen(), newLicenseVerifier(), newLicenseGen(), newLicenseCheck(),
		newLicenseDevice(), newLicenseActivate(), newLicenseServer())
	return root
}

// newLicenseKeygen creates a fresh master secret.
func newLicenseKeygen() *cobra.Command {
	var out string
	var force bool
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "生成主密钥（只需一次）",
		Long: "生成主密钥并写入文件。\n\n" +
			"文件内容既是签发凭据也是构建凭据，权限写成 0600。丢失之后无法再为已售出的\n" +
			"客户续期（他们的密钥仍能验证，但你签不出同族的新密钥），请自行备份。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(out) == "" {
				return errors.New("--out 必填，例如 --out ~/.tdl-issuer.txt")
			}
			path, err := expandHome(out)
			if err != nil {
				return err
			}
			if !force {
				if _, statErr := os.Stat(path); statErr == nil {
					return fmt.Errorf("%s 已存在；确认要覆盖请加 --force（旧密钥一旦覆盖，已签发客户的续期就断了）", path)
				}
			}
			master, err := license.NewMasterKey()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("创建目录: %w", err)
			}
			if err := os.WriteFile(path, []byte(license.EncodeMaster(master)+"\n"), 0o600); err != nil {
				return fmt.Errorf("写入主密钥: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "主密钥已写入 %s（权限 600）\n\n", path)
			fmt.Fprintf(out, "  %s\n\n", license.EncodeMaster(master))
			fmt.Fprintln(out, "下面两行在编译软件时注入，客户拿到的是公钥和校验串，不是主密钥：")
			fmt.Fprintf(out, "\n  -X github.com/iyear/tdl/license.hmacKeyB64=%s\n", license.EncodeHMACKey(master))
			fmt.Fprintf(out, "  -X github.com/iyear/tdl/license.pubKeyB64=%s\n", license.EncodePublicKey(master))
			fmt.Fprintln(out, "\n（用 `tdl license verifier --key-file "+path+"` 可随时重新打印这两行。）")
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "主密钥文件路径，例如 ~/.tdl-issuer.txt")
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已存在的主密钥文件")
	return cmd
}

// newLicenseVerifier prints the -ldflags strings for a build.
func newLicenseVerifier() *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "verifier",
		Short: "打印编译时注入的校验材料",
		RunE: func(cmd *cobra.Command, _ []string) error {
			master, err := readMaster(keyFile)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "-X github.com/iyear/tdl/license.hmacKeyB64=%s\n", license.EncodeHMACKey(master))
			fmt.Fprintf(out, "-X github.com/iyear/tdl/license.pubKeyB64=%s\n", license.EncodePublicKey(master))
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key-file", "", "主密钥文件")
	_ = cmd.MarkFlagRequired("key-file")
	return cmd
}

// newLicenseGen issues one or more customer keys.
func newLicenseGen() *cobra.Command {
	var keyFile, device string
	var days, count int
	var forever, strong bool
	cmd := &cobra.Command{
		Use:   "gen",
		Short: "为客户签发密钥",
		Long: "为客户签发密钥。\n\n" +
			"--device 填客户在激活页看到的 8 位设备码，这个密钥就只能在那台机器上用；\n" +
			"填 any 则首次激活时自动绑定到那台机器。前者更严（换机即失效），后者更好卖\n" +
			"（不用先问客户要设备码）。\n\n" +
			"--days 是有效天数，--forever 是永久密钥，两者互斥。",
		Example: "  tdl license gen --key-file ~/.tdl-issuer.txt --device 7K3M9QX2 --days 30\n" +
			"  tdl license gen --key-file ~/.tdl-issuer.txt --device any --forever\n" +
			"  tdl license gen --key-file ~/.tdl-issuer.txt --device 7K3M9QX2 --days 365 --strong",
		RunE: func(cmd *cobra.Command, _ []string) error {
			master, err := readMaster(keyFile)
			if err != nil {
				return err
			}
			if forever && days > 0 {
				return errors.New("--days 与 --forever 只能选一个")
			}
			if !forever && days <= 0 {
				return errors.New("请用 --days N 指定有效天数，或用 --forever 签发永久密钥")
			}
			var dev uint64
			switch strings.ToLower(strings.TrimSpace(device)) {
			case "", "any", "auto":
				dev = 0
			default:
				c, err := license.ParseDeviceCode(device)
				if err != nil {
					return fmt.Errorf("设备码无效：%w（应形如 7K3M9QX2）", err)
				}
				dev = c
			}
			if count <= 0 {
				count = 1
			}
			out := cmd.OutOrStdout()
			for i := 0; i < count; i++ {
				var (
					key string
					p   license.Payload
					err error
				)
				if strong {
					key, p, err = license.IssueStrong(master, dev, days)
				} else {
					key, p, err = license.IssueShort(master, dev, days)
				}
				if err != nil {
					return err
				}
				fmt.Fprintln(out, key)
				fmt.Fprintf(out, "    序列号 %s · ", encodeSerial(p.Serial))
				if p.Forever() {
					fmt.Fprint(out, "永久")
				} else {
					fmt.Fprintf(out, "至 %s（%d 天）", p.ExpiresAt().Format("2006-01-02"), days)
				}
				if dev == 0 {
					fmt.Fprintln(out, " · 首激绑定设备")
				} else {
					fmt.Fprintf(out, " · 绑定设备 %s\n", license.FormatDeviceCode(dev))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key-file", "", "主密钥文件")
	cmd.Flags().StringVar(&device, "device", "any", "客户设备码（激活页显示），或 any 表示首激绑定")
	cmd.Flags().IntVar(&days, "days", 0, "有效天数")
	cmd.Flags().BoolVar(&forever, "forever", false, "签发永久密钥")
	cmd.Flags().BoolVar(&strong, "strong", false, "签发 Ed25519 强密钥（更长，无法被逆向伪造）")
	cmd.Flags().IntVar(&count, "count", 1, "一次签发多少个")
	_ = cmd.MarkFlagRequired("key-file")
	return cmd
}

// newLicenseCheck validates a key against the given master.
func newLicenseCheck() *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "check <密钥>",
		Short: "校验一个密钥并打印其内容",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			master, err := readMaster(keyFile)
			if err != nil {
				return err
			}
			v, err := license.NewVerifier(license.EncodeHMACKey(master), license.EncodePublicKey(master))
			if err != nil {
				return err
			}
			p, kind, err := v.Parse(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "格式      %s\n", kind)
			fmt.Fprintf(out, "序列号    %s\n", encodeSerial(p.Serial))
			if p.Device == 0 {
				fmt.Fprintln(out, "设备      首激绑定")
			} else {
				fmt.Fprintf(out, "设备      %s\n", license.FormatDeviceCode(p.Device))
			}
			if p.Forever() {
				fmt.Fprintln(out, "有效期    永久")
			} else {
				fmt.Fprintf(out, "有效期    至 %s（剩余 %d 天）\n",
					p.ExpiresAt().Format("2006-01-02"), p.DaysLeft(nowUTC()))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key-file", "", "主密钥文件")
	_ = cmd.MarkFlagRequired("key-file")
	return cmd
}

// newLicenseDevice prints this machine's device code.
func newLicenseDevice() *cobra.Command {
	return &cobra.Command{
		Use:   "device",
		Short: "打印本机设备码",
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, parts, err := license.LocalDeviceCode()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n", license.FormatDeviceCode(code))
			fmt.Fprintf(out, "    由以下材料派生：%s\n", strings.Join(parts, " · "))
			return nil
		},
	}
}

// readMaster loads the master key file, tolerating the trailing newline that
// every editor and shell redirect adds.
func readMaster(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("--key-file 必填")
	}
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取主密钥 %s: %w", p, err)
	}
	return license.ParseMaster(strings.TrimSpace(string(b)))
}

// encodeSerial renders a serial the same way the licence itself does.
func encodeSerial(v uint64) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	buf := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		buf[i] = alphabet[v&0x1f]
		v >>= 5
	}
	return string(buf)
}

// expandHome resolves a leading ~ so the flag behaves like a shell path.
func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定家目录: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// nowUTC is a seam so tests can control time if this ever grows one.
func nowUTC() time.Time { return time.Now().UTC() }
