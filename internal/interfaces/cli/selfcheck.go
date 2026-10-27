package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/newapi"
)

// selfcheck 执行 M1 自检（开发文档 §10）并打印报告；有失败项时返回错误，便于脚本判断。
func (e *env) selfcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fixtures := fs.String("fixtures", "", "把脱敏后的响应写进这个目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(e.configPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateUpstream(); err != nil {
		return err
	}
	up := cfg.Upstream
	client := newapi.New(newapi.Options{BaseURL: up.BaseURL, AccessToken: up.AccessToken, UserID: up.UserID,
		Timeout: up.Timeout, MinInterval: up.MinRequestInterval})
	now := time.Now()
	fmt.Fprintf(e.stderr, "自检 %s（用户 %d），请求间隔 %s，约需 3～5 分钟\n", up.BaseURL, up.UserID, up.MinRequestInterval)
	report := newapi.SelfCheck(ctx, client, newapi.SelfCheckOptions{
		Now: now, Lag: cfg.Sync.Lag, Location: cfg.Location(), Progress: e.stderr,
	})

	fmt.Fprintf(e.stdout, "\nM1 自检结果（%s，%s）\n\n", up.BaseURL, now.In(cfg.Location()).Format("2006-01-02 15:04"))
	for _, c := range report.Checks {
		step := fmt.Sprintf("%2d.", c.Step)
		if c.Step == 0 {
			step = " --"
		}
		fmt.Fprintf(e.stdout, "%s [%s] %s\n      %s\n", step, c.Level, c.Title, c.Detail)
	}
	fmt.Fprintf(e.stdout, "11. [%s] 样本\n      %s\n", fixtureLevel(*fixtures, report), e.writeFixtures(*fixtures, report))
	printSummary(e.stdout, report)

	switch {
	case ctx.Err() != nil:
		return errors.New("自检被中断")
	case report.Failed():
		return errors.New("自检有失败项，修正后重跑")
	}
	return nil
}

func fixtureLevel(dir string, report *newapi.SelfCheckReport) newapi.Level {
	if dir == "" || len(report.Fixtures) == 0 {
		return newapi.Skip
	}
	return newapi.Pass
}

// writeFixtures 把样本写进 dir，返回给报告的说明。
func (e *env) writeFixtures(dir string, report *newapi.SelfCheckReport) string {
	switch {
	case dir == "":
		return "没带 -fixtures，不写样本"
	case len(report.Fixtures) == 0:
		return "没有可写的样本"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "创建目录失败：" + err.Error()
	}
	names := make([]string, 0, len(report.Fixtures))
	for name := range report.Fixtures {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), report.Fixtures[name], 0o644); err != nil {
			return "写入失败：" + err.Error()
		}
	}
	return fmt.Sprintf("已脱敏写入 %s：%v；提交前再看一眼", dir, names)
}

func printSummary(w io.Writer, report *newapi.SelfCheckReport) {
	count := map[newapi.Level]int{}
	for _, c := range report.Checks {
		count[c.Level]++
	}
	fmt.Fprintf(w, "\n结论：通过 %d、注意 %d、失败 %d、信息 %d、跳过 %d。",
		count[newapi.Pass], count[newapi.Note], count[newapi.Fail], count[newapi.Info], count[newapi.Skip])
	if count[newapi.Note] > 0 {
		fmt.Fprint(w, "「注意」项与开发文档的假设不同，先改文档再写同步。")
	}
	fmt.Fprintln(w)
}
