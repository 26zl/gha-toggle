package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/google/go-github/v68/github"
	"golang.org/x/sync/errgroup"
)

const defaultConcurrency = 8

const dynamicPrefix = "dynamic/"

type StateEntry struct {
	Repo string `json:"repo"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type opts struct {
	dryRun         bool
	owner          string
	includeForks   bool
	includeDynamic bool
	concurrency    int
	jsonOut        bool
	stateFile      string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	o := &opts{concurrency: defaultConcurrency}
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.StringVar(&o.owner, "owner", "", "")
	fs.BoolVar(&o.includeForks, "include-forks", false, "")
	fs.BoolVar(&o.includeDynamic, "include-dynamic", false, "")
	fs.IntVar(&o.concurrency, "concurrency", defaultConcurrency, "")
	fs.BoolVar(&o.jsonOut, "json", false, "")
	fs.StringVar(&o.stateFile, "state-file", "", "")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if o.concurrency < 1 {
		o.concurrency = 1
	}
	args := fs.Args()
	ctx := context.Background()

	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return
	}

	client, err := newClient()
	if err != nil {
		die(err)
	}

	switch cmd {
	case "list":
		err = cmdList(ctx, client, o)
	case "disable-all":
		err = cmdDisableAll(ctx, client, o)
	case "enable-all":
		err = cmdEnableAll(ctx, client, o)
	case "enable-all-disabled":
		err = cmdEnableAllDisabled(ctx, client, o)
	case "disable-repo":
		err = cmdToggleRepo(ctx, client, o, args, "disable")
	case "enable-repo":
		err = cmdToggleRepo(ctx, client, o, args, "enable")
	case "status":
		err = cmdStatus(ctx, client, o)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		die(err)
	}
}

func usage() {
	fmt.Print(`Usage: gha-toggle <command> [options]

Commands:
  list                       List all workflows across all your repos
  disable-all                Disable every active workflow, save state file
  enable-all                 Re-enable workflows from saved state file
  enable-all-disabled        Re-enable EVERY currently-disabled workflow
  disable-repo <owner/repo>  Disable all active workflows in one repo
  enable-repo  <owner/repo>  Re-enable all disabled workflows in one repo
  status                     Show billing + workflow counts across repos
  help                       Show this help

Options:
  --dry-run                  Print actions without calling the API
  --owner <login>            Only touch repos owned by this user/org
  --include-forks            Include forked repos (default: skipped)
  --include-dynamic          Include dynamic/ workflows (CodeQL, Dependabot,
                             Copilot — these always 422 on toggle, so are
                             skipped by default)
  --concurrency <n>          Parallel API calls (default: 8)
  --json                     Emit JSON (list, status)
  --state-file <path>        Override default state file location

Env:
  GHA_STATE_DIR              State directory (default: ~/.gha-toggle)
`)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func newClient() (*github.Client, error) {
	token, _ := auth.TokenForHost("github.com")
	if token == "" {
		return nil, errors.New("no GitHub token found — run: gh auth login")
	}
	return github.NewClient(nil).WithAuthToken(token), nil
}

func splitRepo(fullName string) (owner, name string) {
	i := strings.IndexByte(fullName, '/')
	if i < 0 {
		return "", fullName
	}
	return fullName[:i], fullName[i+1:]
}

func skipWorkflow(wf *github.Workflow, o *opts) bool {
	if o.includeDynamic {
		return false
	}
	return strings.HasPrefix(wf.GetPath(), dynamicPrefix)
}

func listRepos(ctx context.Context, c *github.Client, o *opts) ([]*github.Repository, error) {
	var all []*github.Repository
	opts := &github.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner,collaborator,organization_member",
		Visibility:  "all",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		repos, resp, err := c.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}
		all = append(all, repos...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	out := all[:0]
	for _, r := range all {
		if o.owner != "" && !strings.EqualFold(r.GetOwner().GetLogin(), o.owner) {
			continue
		}
		if r.GetArchived() {
			continue
		}
		if r.GetFork() && !o.includeForks {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetFullName() < out[j].GetFullName() })
	return out, nil
}

func listWorkflows(ctx context.Context, c *github.Client, owner, name string) ([]*github.Workflow, error) {
	var all []*github.Workflow
	opts := &github.ListOptions{PerPage: 100}
	for {
		wfs, resp, err := c.Actions.ListWorkflows(ctx, owner, name, opts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, err
		}
		all = append(all, wfs.Workflows...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

type listRow struct {
	State string `json:"state"`
	Repo  string `json:"repo"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

func collectRows(ctx context.Context, c *github.Client, o *opts, repos []*github.Repository) ([]listRow, error) {
	var (
		mu   sync.Mutex
		rows []listRow
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(o.concurrency)
	for _, r := range repos {
		r := r
		g.Go(func() error {
			owner, name := splitRepo(r.GetFullName())
			wfs, err := listWorkflows(gctx, c, owner, name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: %s: %v\n", r.GetFullName(), err)
				return nil
			}
			mu.Lock()
			for _, wf := range wfs {
				rows = append(rows, listRow{wf.GetState(), r.GetFullName(), wf.GetName(), wf.GetPath()})
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

func cmdList(ctx context.Context, c *github.Client, o *opts) error {
	repos, err := listRepos(ctx, c, o)
	if err != nil {
		return err
	}
	rows, err := collectRows(ctx, c, o, repos)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	fmt.Println("STATE\tREPO\tWORKFLOW\tPATH")
	for _, r := range rows {
		fmt.Printf("%s\t%s\t%s\t%s\n", r.State, r.Repo, r.Name, r.Path)
	}
	return nil
}

func cmdDisableAll(ctx context.Context, c *github.Client, o *opts) error {
	repos, err := listRepos(ctx, c, o)
	if err != nil {
		return err
	}

	statePath, err := stateFilePath(o)
	if err != nil {
		return err
	}
	existing, err := loadState(statePath)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, e := range existing {
		have[entryKey(e.Repo, e.ID)] = true
	}

	var (
		mu      sync.Mutex
		added   []StateEntry
		changed int
		failed  int
		skipped int
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(o.concurrency)
	for _, r := range repos {
		r := r
		g.Go(func() error {
			owner, name := splitRepo(r.GetFullName())
			wfs, err := listWorkflows(gctx, c, owner, name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: %s: %v\n", r.GetFullName(), err)
				return nil
			}
			for _, wf := range wfs {
				if wf.GetState() != "active" {
					continue
				}
				if skipWorkflow(wf, o) {
					mu.Lock()
					skipped++
					mu.Unlock()
					continue
				}
				fmt.Printf("disable  %s :: %s\n", r.GetFullName(), wf.GetName())
				if !o.dryRun {
					if _, err := c.Actions.DisableWorkflowByID(gctx, owner, name, wf.GetID()); err != nil {
						fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
						mu.Lock()
						failed++
						mu.Unlock()
						continue
					}
				}
				e := StateEntry{Repo: r.GetFullName(), ID: wf.GetID(), Name: wf.GetName(), Path: wf.GetPath()}
				mu.Lock()
				changed++
				if !have[entryKey(e.Repo, e.ID)] {
					added = append(added, e)
					have[entryKey(e.Repo, e.ID)] = true
				}
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	if o.dryRun {
		fmt.Printf("\ndry-run: would disable %d workflows (%d new state entries, %d dynamic skipped)\n",
			changed, len(added), skipped)
		return nil
	}

	merged := append(existing, added...)
	if err := saveState(statePath, merged); err != nil {
		return err
	}
	fmt.Printf("\ndisabled %d workflows (%d new state entries, %d failed, %d dynamic skipped); state has %d entries: %s\n",
		changed, len(added), failed, skipped, len(merged), statePath)
	return nil
}

func cmdEnableAll(ctx context.Context, c *github.Client, o *opts) error {
	statePath, err := stateFilePath(o)
	if err != nil {
		return err
	}
	entries, err := loadState(statePath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("state file empty or missing: %s — run disable-all first, or use enable-all-disabled", statePath)
	}

	var (
		mu     sync.Mutex
		ok     int
		failed []StateEntry
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(o.concurrency)
	for _, e := range entries {
		e := e
		g.Go(func() error {
			owner, name := splitRepo(e.Repo)
			fmt.Printf("enable   %s :: %s\n", e.Repo, e.Name)
			if o.dryRun {
				return nil
			}
			_, err := c.Actions.EnableWorkflowByID(gctx, owner, name, e.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
				failed = append(failed, e)
				return nil
			}
			ok++
			return nil
		})
	}
	_ = g.Wait()

	if o.dryRun {
		fmt.Printf("\ndry-run: would enable %d workflows\n", len(entries))
		return nil
	}

	if err := saveState(statePath, failed); err != nil {
		return err
	}
	fmt.Printf("\nenabled %d workflows (%d failed; remaining state: %d)\n", ok, len(failed), len(failed))
	return nil
}

func cmdEnableAllDisabled(ctx context.Context, c *github.Client, o *opts) error {
	repos, err := listRepos(ctx, c, o)
	if err != nil {
		return err
	}
	var (
		mu      sync.Mutex
		changed int
		failed  int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(o.concurrency)
	for _, r := range repos {
		r := r
		g.Go(func() error {
			owner, name := splitRepo(r.GetFullName())
			wfs, err := listWorkflows(gctx, c, owner, name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: %s: %v\n", r.GetFullName(), err)
				return nil
			}
			for _, wf := range wfs {
				if !strings.HasPrefix(wf.GetState(), "disabled") {
					continue
				}
				if skipWorkflow(wf, o) {
					continue
				}
				fmt.Printf("enable   %s :: %s (%s)\n", r.GetFullName(), wf.GetName(), wf.GetState())
				if o.dryRun {
					mu.Lock()
					changed++
					mu.Unlock()
					continue
				}
				if _, err := c.Actions.EnableWorkflowByID(gctx, owner, name, wf.GetID()); err != nil {
					fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				mu.Lock()
				changed++
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if o.dryRun {
		fmt.Printf("\ndry-run: would enable %d workflows\n", changed)
		return nil
	}
	fmt.Printf("\nenabled %d workflows (%d failed)\n", changed, failed)
	return nil
}

func cmdToggleRepo(ctx context.Context, c *github.Client, o *opts, args []string, action string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gha-toggle %s-repo <owner/repo>", action)
	}
	owner, name := splitRepo(args[0])
	if owner == "" || name == "" {
		return fmt.Errorf("invalid repo %q (expected owner/repo)", args[0])
	}
	wfs, err := listWorkflows(ctx, c, owner, name)
	if err != nil {
		return err
	}

	wantState := "active"
	if action == "enable" {
		wantState = "disabled"
	}

	count, failed := 0, 0
	for _, wf := range wfs {
		state := wf.GetState()
		match := state == wantState
		if action == "enable" {
			match = strings.HasPrefix(state, "disabled")
		}
		if !match {
			continue
		}
		if skipWorkflow(wf, o) {
			continue
		}
		fmt.Printf("%s   %s/%s :: %s\n", action, owner, name, wf.GetName())
		if o.dryRun {
			count++
			continue
		}
		var apiErr error
		if action == "disable" {
			_, apiErr = c.Actions.DisableWorkflowByID(ctx, owner, name, wf.GetID())
		} else {
			_, apiErr = c.Actions.EnableWorkflowByID(ctx, owner, name, wf.GetID())
		}
		if apiErr != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", apiErr)
			failed++
			continue
		}
		count++
	}
	if o.dryRun {
		fmt.Printf("\ndry-run: would %s %d workflows\n", action, count)
		return nil
	}
	fmt.Printf("\n%sd %d workflows (%d failed)\n", action, count, failed)
	return nil
}

type statusOutput struct {
	User    string         `json:"user"`
	Billing map[string]any `json:"billing,omitempty"`
	Repos   int            `json:"repos"`
	States  map[string]int `json:"states"`
}

func cmdStatus(ctx context.Context, c *github.Client, o *opts) error {
	user, _, err := c.Users.Get(ctx, "")
	if err != nil {
		return err
	}
	out := statusOutput{User: user.GetLogin(), States: map[string]int{}}

	if billing, _, err := c.Billing.GetActionsBillingUser(ctx, user.GetLogin()); err == nil {
		bj, _ := json.Marshal(billing)
		_ = json.Unmarshal(bj, &out.Billing)
	}

	repos, err := listRepos(ctx, c, o)
	if err != nil {
		return err
	}
	out.Repos = len(repos)
	rows, err := collectRows(ctx, c, o, repos)
	if err != nil {
		return err
	}
	for _, r := range rows {
		out.States[r.State]++
	}

	if o.jsonOut {
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Println("user:", out.User)
	fmt.Println()
	fmt.Println("Actions billing:")
	if out.Billing != nil {
		if v, ok := out.Billing["total_minutes_used"]; ok {
			fmt.Printf("  minutes used:      %v / %v\n", v, out.Billing["included_minutes"])
		}
		if v, ok := out.Billing["total_paid_minutes_used"]; ok {
			fmt.Printf("  paid minutes used: %v\n", v)
		}
		if bd, ok := out.Billing["minutes_used_breakdown"]; ok {
			b, _ := json.MarshalIndent(bd, "    ", "  ")
			fmt.Printf("  breakdown:\n    %s\n", b)
		}
	} else {
		fmt.Println("  (could not fetch — token may need user scope: gh auth refresh -s user)")
	}
	fmt.Println()
	fmt.Printf("Repos scanned: %d\n", out.Repos)
	fmt.Println("Workflows by state:")
	keys := make([]string, 0, len(out.States))
	for k := range out.States {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-20s %d\n", k+":", out.States[k])
	}
	return nil
}

func stateFilePath(o *opts) (string, error) {
	if o != nil && o.stateFile != "" {
		if dir := filepath.Dir(o.stateFile); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
		}
		return o.stateFile, nil
	}
	dir := os.Getenv("GHA_STATE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".gha-toggle")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "enabled-workflows.json"), nil
}

func loadState(path string) ([]StateEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var entries []StateEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	return entries, nil
}

func saveState(path string, entries []StateEntry) error {
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func entryKey(repo string, id int64) string {
	return fmt.Sprintf("%s#%d", repo, id)
}
