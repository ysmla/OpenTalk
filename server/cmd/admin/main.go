// Command admin performs the operations that must work before any administrator exists: creating or
// promoting an administrator, and configuring SMTP.
//
// It intentionally offers only what bootstrap needs. Day-to-day moderation happens through the admin
// API, where changes are audited against a real actor.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"opentalk/server/internal/account"
	"opentalk/server/internal/auth"
	"opentalk/server/internal/content"
	"opentalk/server/internal/platform/config"
	"opentalk/server/internal/platform/database"
	"opentalk/server/internal/platform/secretbox"
	"opentalk/server/internal/settings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误："+err.Error())
		os.Exit(1)
	}
}

func run() error {
	action := flag.String("action", "", "create-admin、promote、set-smtp、clear-smtp、seed-boards 或 show")
	email := flag.String("email", "", "目标账号邮箱")
	nickname := flag.String("nickname", "", "create-admin 使用的昵称")
	host := flag.String("host", "", "SMTP 主机")
	port := flag.Int("port", 0, "SMTP 端口")
	security := flag.String("security", "", "SMTP 加密方式：none、starttls 或 tls")
	username := flag.String("username", "", "SMTP 认证账号")
	from := flag.String("from", "", "发信地址")
	fromName := flag.String("from-name", "", "发信人显示名")
	passwordStdin := flag.Bool("password-stdin", false, "从标准输入读取密码（不回显；建议用管道而不是手输）")
	reason := flag.String("reason", "命令行配置变更", "写入审计原因的说明")
	actorEmail := flag.String("actor-email", "", "把设置变更记到哪个已注册管理员名下；省略则不写审计记录")
	flag.Parse()

	provided := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	reader := bufio.NewReader(os.Stdin)
	users := auth.Store{DB: db}

	patch := settings.Patch{
		SMTPHost:        optionalString(provided, "host", *host),
		SMTPPort:        optionalInt(provided, "port", *port),
		Security:        optionalString(provided, "security", *security),
		SMTPUsername:    optionalString(provided, "username", *username),
		SMTPFromAddress: optionalString(provided, "from", *from),
		SMTPFromName:    optionalString(provided, "from-name", *fromName),
	}

	switch *action {
	case "create-admin":
		return createAdmin(ctx, db, users, reader, *email, *nickname)
	case "promote":
		return promote(ctx, db, *email)
	case "set-smtp":
		return setSMTP(ctx, db, users, cfg, reader, patch, *passwordStdin, *reason, *actorEmail)
	case "clear-smtp":
		return clearSMTP(ctx, db, users, cfg, *reason, *actorEmail)
	case "seed-boards":
		return seedBoards(ctx, db)
	case "show":
		return show(ctx, db, cfg)
	default:
		flag.Usage()
		return errors.New("必须指定 -action")
	}
}

// createAdmin creates a verified administrator directly. This bypasses email verification, which is
// exactly why it is a local command rather than an HTTP endpoint: only someone with server and
// database access can run it.
func createAdmin(ctx context.Context, db *sql.DB, users auth.Store, reader *bufio.Reader, rawEmail, rawNickname string) error {
	email, err := account.NormalizeEmail(rawEmail)
	if err != nil {
		return errors.New("邮箱格式无效")
	}
	nickname := strings.TrimSpace(rawNickname)
	if err := account.ValidateNickname(nickname); err != nil {
		return errors.New("昵称无效：只能包含字母和汉字，且不超过 10 个字符")
	}
	promptPassword(reader, "请输入新管理员密码")
	password, err := readSecret(reader)
	if err != nil {
		return err
	}
	if err := account.ValidatePassword(password); err != nil {
		return errors.New("密码无效：必须为 8 到 16 个字符并同时包含字母和数字")
	}
	passwordHash, err := account.HashPassword(password)
	if err != nil {
		return err
	}
	publicID, err := account.NewPublicID()
	if err != nil {
		return err
	}
	if _, _, err := users.UserByEmail(ctx, email); err == nil {
		return errors.New("该邮箱已注册；如需提权请使用 -action=promote")
	} else if !errors.Is(err, auth.ErrUserNotFound) {
		return err
	}
	id, err := users.CreateUser(ctx, auth.NewUser{
		Email:        email,
		PasswordHash: passwordHash,
		Nickname:     nickname,
		PublicID:     publicID,
		Role:         "admin",
	})
	if err != nil {
		return err
	}
	fmt.Printf("已创建管理员：id=%d public_id=%s\n", id, publicID)
	fmt.Println("该账号已跳过邮箱验证；请提醒本人首次登录后立即修改密码。")
	return nil
}

// promote grants the admin role to an existing account.
func promote(ctx context.Context, db *sql.DB, rawEmail string) error {
	email, err := account.NormalizeEmail(rawEmail)
	if err != nil {
		return errors.New("邮箱格式无效")
	}
	result, err := db.ExecContext(ctx, `UPDATE users SET role='admin' WHERE email_normalized=? AND role<>'admin'`, email)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("没有可提权的账号：邮箱未注册，或该账号已是管理员")
	}
	fmt.Printf("已把 %s 提升为管理员\n", email)
	return nil
}

// setSMTP writes SMTP settings. The password is encrypted with a key derived from APP_SECRET, so the
// database only ever holds ciphertext.
func setSMTP(ctx context.Context, db *sql.DB, users auth.Store, cfg config.Config, reader *bufio.Reader, patch settings.Patch, readPassword bool, reason, actorEmail string) error {
	if readPassword {
		promptPassword(reader, "请输入 SMTP 密码（直接回车表示清除已保存的密码）")
		password, err := readSecret(reader)
		if err != nil {
			return err
		}
		patch.SMTPPassword = &password
	}
	if patch.Empty() {
		return errors.New("没有提供任何要修改的设置")
	}
	key, err := secretbox.Derive(cfg.AppSecret, "settings-secret")
	if err != nil {
		return err
	}
	var actorID uint64
	if strings.TrimSpace(actorEmail) != "" {
		email, err := account.NormalizeEmail(actorEmail)
		if err != nil {
			return errors.New("操作者邮箱格式无效")
		}
		user, _, err := users.UserByEmail(ctx, email)
		if err != nil {
			return errors.New("找不到作为操作者的账号，无法写审计记录")
		}
		actorID = user.ID
	}
	updated, err := settings.Store{DB: db, Key: key}.Update(ctx, actorID, patch, reason)
	if err != nil {
		return err
	}
	if actorID == 0 {
		fmt.Println("提示：未指定 -actor-email，本次变更没有写入审计记录。")
	}
	printSettings(updated)
	return nil
}

// seedBoards inserts the first-version boards when the installation has none. Boards are business
// data an administrator may rename or archive, so they are seeded by a command rather than by a
// migration; running it again on a populated instance changes nothing.
func seedBoards(ctx context.Context, db *sql.DB) error {
	existing, err := content.Store{DB: db}.SeedBoards(ctx)
	if err != nil {
		return err
	}
	if existing > 0 {
		fmt.Printf("已有 %d 个板块，未做改动（种子命令只在空安装上生效）\n", existing)
		return nil
	}
	fmt.Println("已预置板块：综合讨论（讨论板块）、问答（Q&A 板块）")
	return nil
}

// clearSMTP removes the SMTP configuration in one step. Without it an operator who mistyped a port
// could not undo it: the port has no "unset" value that the normal patch or the admin API accepts.
func clearSMTP(ctx context.Context, db *sql.DB, users auth.Store, cfg config.Config, reason, actorEmail string) error {
	key, err := secretbox.Derive(cfg.AppSecret, "settings-secret")
	if err != nil {
		return err
	}
	var actorID uint64
	if strings.TrimSpace(actorEmail) != "" {
		email, err := account.NormalizeEmail(actorEmail)
		if err != nil {
			return errors.New("操作者邮箱格式无效")
		}
		user, _, err := users.UserByEmail(ctx, email)
		if err != nil {
			return errors.New("找不到作为操作者的账号，无法写审计记录")
		}
		actorID = user.ID
	}
	empty := ""
	updated, err := settings.Store{DB: db, Key: key}.Update(ctx, actorID, settings.Patch{
		SMTPHost:        &empty,
		ClearSMTPPort:   true,
		SMTPUsername:    &empty,
		SMTPPassword:    &empty,
		SMTPFromAddress: &empty,
		SMTPFromName:    &empty,
	}, reason)
	if err != nil {
		return err
	}
	if actorID == 0 {
		fmt.Println("提示：未指定 -actor-email，本次变更没有写入审计记录。")
	}
	printSettings(updated)
	return nil
}

// show prints the effective configuration. The SMTP password is never printed, only whether one is
// stored, because terminal scrollback is not a safe place for a credential.
func show(ctx context.Context, db *sql.DB, cfg config.Config) error {
	key, err := secretbox.Derive(cfg.AppSecret, "settings-secret")
	if err != nil {
		return err
	}
	current, err := settings.Store{DB: db, Key: key}.Load(ctx)
	if err != nil {
		return err
	}
	printSettings(current)
	return nil
}

func printSettings(s settings.Settings) {
	host := s.SMTPHost
	if host == "" {
		host = "（未配置）"
	}
	password := "未配置"
	if s.SMTPPasswordSet {
		password = "已配置（不回显）"
	}
	fmt.Printf("SMTP 主机：%s\n", host)
	fmt.Printf("SMTP 端口：%d\n", s.SMTPPort)
	fmt.Printf("SMTP 加密：%s\n", s.Security)
	fmt.Printf("SMTP 账号：%s\n", s.SMTPUsername)
	fmt.Printf("SMTP 密码：%s\n", password)
	fmt.Printf("发信地址：%s\n", s.SMTPFromAddress)
	fmt.Printf("发信人名：%s\n", s.SMTPFromName)
	fmt.Printf("图形验证码有效期：%d 秒\n", s.CaptchaTTLSeconds)
	fmt.Printf("图形验证码最大提交次数：%d\n", s.CaptchaMaxAttempts)
	fmt.Printf("邮件验证码有效期：%d 秒\n", s.EmailCodeTTLSeconds)
	fmt.Printf("邮件验证码最大校验次数：%d\n", s.EmailCodeMaxAttempts)
	fmt.Printf("邮件验证码重发间隔：%d 秒\n", s.EmailCodeResendIntervalSeconds)
	fmt.Printf("每邮箱每小时发送上限：%d\n", s.EmailCodeEmailHourlyLimit)
	fmt.Printf("每 IP 每小时发送上限：%d\n", s.EmailCodeIPHourlyLimit)
	fmt.Printf("登录连续失败阈值：%d\n", s.LoginCaptchaFailureThreshold)
	fmt.Printf("单张图片字节上限：%d\n", s.MediaMaxBytes)
	fmt.Printf("图片像素上限：%d\n", s.MediaMaxDimension)
	fmt.Printf("每篇内容图片张数上限：%d\n", s.MediaMaxPerContent)
	fmt.Printf("待用附件保留小时数：%d\n", s.MediaPendingTTLHours)
	fmt.Printf("孤儿附件清理天数：%d\n", s.MediaOrphanCleanupDays)
	fmt.Printf("最近更新：%s\n", s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
}

// promptPassword warns that interactive input echoes. Disabling echo needs a terminal dependency the
// project does not carry, so piping the value in is the recommended path.
func promptPassword(reader *bufio.Reader, label string) {
	if reader.Buffered() > 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s（输入会回显，建议改用管道）：", label)
}

// readSecret reads one line from stdin. The value is never echoed back or logged.
func readSecret(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func optionalString(provided map[string]bool, name, value string) *string {
	if !provided[name] {
		return nil
	}
	return &value
}

func optionalInt(provided map[string]bool, name string, value int) *int {
	if !provided[name] {
		return nil
	}
	return &value
}
