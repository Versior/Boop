package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"boop/internal/auth"
	"golang.org/x/term"
)

// isTerminal and readPassword wrap x/term so the terminal dependency stays in
// one place.
func isTerminal(fd int) bool { return term.IsTerminal(fd) }

func readPassword(fd int) ([]byte, error) { return term.ReadPassword(fd) }

// passwordPrompter asks for one secret without echoing it. It is injected so the
// command can be tested without a terminal.
type passwordPrompter func(label string) (string, error)

// runInitOwner creates the single owner account. It refuses once an owner
// exists, so it can never hand the site to a second person.
func runInitOwner(args []string, stdout io.Writer, prompt passwordPrompter) error {
	flags := flag.NewFlagSet("init-owner", flag.ContinueOnError)
	flags.SetOutput(stdout)
	email := flags.String("email", "", "站长登录邮箱")
	name := flags.String("name", "", "站长昵称")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("init-owner: 必须提供 --email")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("init-owner: 必须提供 --name")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	_, db, err := openStore(logger)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	exists, err := auth.OwnerExists(ctx, db)
	if err != nil {
		return err
	}
	if exists {
		return auth.ErrOwnerExists
	}

	password, err := prompt("请输入站长密码（输入不回显）：")
	if err != nil {
		return err
	}
	confirmation, err := prompt("请再次输入密码：")
	if err != nil {
		return err
	}
	if password != confirmation {
		return errors.New("init-owner: 两次输入的密码不一致，未创建任何账号")
	}

	owner, err := auth.BootstrapOwner(ctx, db, *email, *name, password)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已创建站长账号：%s（%s）\n", owner.Email, owner.DisplayName)
	return nil
}

// promptHiddenPassword reads a password from the terminal with echo disabled.
// A piped stdin has no terminal to hide input, and refusing is safer than
// printing a secret into a log or a shell history file.
func promptHiddenPassword(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return "", errors.New("init-owner: 需要交互式终端才能隐藏输入密码，请在本机终端中直接运行")
	}
	fmt.Fprint(os.Stderr, label)
	raw, err := readPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("init-owner: 读取密码失败: %w", err)
	}
	return string(raw), nil
}
