package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	// bcryptCost 是管理员密码哈希的成本（开发文档 §8）。
	bcryptCost     = 12
	minPasswordLen = 8
)

// hashPassword 读入密码并输出 bcrypt 哈希。终端里交互输入两次、不回显；从管道读时只读第一行。
func (e *env) hashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	password, err := e.readPassword()
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(password) < minPasswordLen {
		return fmt.Errorf("密码至少 %d 个字符", minPasswordLen)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, string(hash))
	fmt.Fprintln(e.stderr, "把上面这行填到 config.yaml 的 auth.admin_password_hash（YAML 里加双引号）")
	return nil
}

func (e *env) readPassword() (string, error) {
	if f, ok := e.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		read := func(prompt string) (string, error) {
			fmt.Fprint(e.stderr, prompt)
			b, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(e.stderr)
			return string(b), err
		}
		first, err := read("密码：")
		if err != nil {
			return "", err
		}
		second, err := read("再输一次：")
		if err != nil {
			return "", err
		}
		if first != second {
			return "", errors.New("两次输入不一致")
		}
		return first, nil
	}
	line, err := bufio.NewReader(e.stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("没有读到密码")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
