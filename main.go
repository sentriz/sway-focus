package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joshuarubin/go-sway"
	"go.senan.xyz/flagconf"
)

const name = "sway-focus"

var statePath string
var configPath string

func init() {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		fmt.Fprintln(os.Stderr, "XDG_RUNTIME_DIR not set")
		os.Exit(1)
	}
	statePath = filepath.Join(runtimeDir, name)

	configDir, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find config dir: %v\n", err)
		os.Exit(1)
	}
	configPath = filepath.Join(configDir, name)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	flag.CommandLine.Init(name, flag.ExitOnError)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  $ %s \n", flag.CommandLine.Name())
		fmt.Fprintf(flag.CommandLine.Output(), "  $ %s list\n", flag.CommandLine.Name())
		fmt.Fprintf(flag.CommandLine.Output(), "  $ %s focus <con>:<browser>:<tab>\n", flag.CommandLine.Name())
		fmt.Fprintf(flag.CommandLine.Output(), "  $ %s back\n", flag.CommandLine.Name())
		fmt.Fprintf(flag.CommandLine.Output(), "\nOptions:\n")
		flag.PrintDefaults()
	}

	var browsers = browsers{}
	flag.Var(browsers, "browser", "browser to track tabs for, as <app id> bruvtab <url>")
	flag.StringVar(&configPath, "config-path", configPath, "overwrite config path to use instead of cli flags")

	flag.Parse()
	flagconf.ParseEnv()
	flagconf.ParseConfig(configPath)

	var location string

	var err error
	switch args := flag.Args(); {
	case match(args):
		err = track(ctx, browsers)
	case match(args, "list"):
		err = list(ctx, browsers)
	case match(args, "focus", &location):
		err = focus(ctx, browsers, parseLocation(location))
	case match(args, "back"):
		err = back(ctx, browsers)
	default:
		flag.Usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func match(args []string, pattern ...any) bool {
	for i, p := range pattern {
		switch p := p.(type) {
		case string:
			if i >= len(args) || args[i] != p {
				return false
			}
		case *string:
			if i >= len(args) {
				return false
			}
			*p = args[i]
		}
	}
	return len(args) == len(pattern)
}

type browsers map[string]browser

// a browser is a source of tabs for the windows of one sway app id
type browser interface {
	windows() [][]tab                    // tabs grouped by the browser's own windows
	activeTab(windowTitle string) string // the tab a window showing this title is on
	focus(tabID string) error
}

type tab struct {
	id    string
	title string
	url   string
}

func (b browsers) String() string {
	return ""
}

func (b browsers) Set(value string) error {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return fmt.Errorf("should be <app id> <browser> ...")
	}
	appID, kind, rest := fields[0], fields[1], strings.Join(fields[2:], " ")

	switch kind {
	case "bruvtab":
		browser, err := bruvtabParseBrowser(rest)
		if err != nil {
			return err
		}
		b[appID] = browser
		return nil
	default:
		return fmt.Errorf("unknown browser %q", kind)
	}
}

type location struct {
	con     int64
	browser string // the sway app id of the browser the tab is in
	tab     string
}

func parseLocation(s string) location {
	var loc location
	con, rest, _ := strings.Cut(s, ":")
	loc.con, _ = strconv.ParseInt(con, 10, 64)
	loc.browser, loc.tab, _ = strings.Cut(rest, ":")
	return loc
}

func formatLocation(loc location) string {
	var con string
	if loc.con > 0 {
		con = strconv.FormatInt(loc.con, 10)
	}
	return fmt.Sprintf("%s:%s:%s", con, loc.browser, loc.tab)
}

func track(ctx context.Context, browsers browsers) error {
	handlr := &handler{
		EventHandler: sway.NoOpEventHandler(),
		browsers:     browsers,
	}

	// sway isn't accepting connections yet when started from its own config
	return retry(ctx, "subscribe", func() error {
		return sway.Subscribe(ctx, handlr, sway.EventTypeWindow)
	})
}

func back(ctx context.Context, browsers browsers) error {
	state, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	return focus(ctx, browsers, parseLocation(strings.TrimSpace(string(state))))
}

func focus(ctx context.Context, browsers browsers, loc location) error {
	if loc.tab != "" {
		browser, ok := browsers[loc.browser]
		if !ok {
			return fmt.Errorf("unknown browser %q", loc.browser)
		}
		if err := browser.focus(loc.tab); err != nil {
			return fmt.Errorf("focus tab: %w", err)
		}
	}
	if loc.con == 0 {
		return nil
	}

	client, err := sway.New(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if _, err := client.RunCommand(ctx, fmt.Sprintf("[con_id=%d] focus", loc.con)); err != nil {
		return fmt.Errorf("run focus: %w", err)
	}
	return nil
}

var matchWorkspace = regexp.MustCompile(`^[0-9]+`)

// list prints every window and browser tab as
// <con>:<browser>:<tab> <workspace> <visible> <app id> <title> <url>, tab separated,
// with each browser window followed by its own tabs
func list(ctx context.Context, browsers browsers) error {
	client, err := sway.New(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	var tabs = map[string][][]tab{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for appID, browser := range browsers {
		wg.Go(func() {
			found := browser.windows()
			mu.Lock()
			defer mu.Unlock()
			tabs[appID] = found
		})
	}

	root, err := client.GetTree(ctx)
	if err != nil {
		return fmt.Errorf("get tree: %w", err)
	}
	wg.Wait()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for workspace := range iterWorkspaces(root) {
		workspaceName := matchWorkspace.FindString(workspace.Name)

		for _, window := range findWindows(workspace) {
			var appID string
			if window.AppID != nil {
				appID = *window.AppID
			}
			var visible string
			if window.Visible != nil && *window.Visible {
				visible = "1"
			}

			// the tab whose title the window is showing is that window's active tab
			var browserWindow []tab
			var activeTab, activeTitle string
			var matched = -1
			for i, browserTabs := range tabs[appID] {
				for _, t := range browserTabs {
					if !titleMatches(window.Name, t.title) || len(t.title) <= len(activeTitle) {
						continue
					}
					browserWindow, activeTab, activeTitle, matched = browserTabs, t.id, t.title, i
				}
			}
			if matched >= 0 {
				tabs[appID] = slices.Delete(tabs[appID], matched, matched+1)
			}

			loc := location{con: window.ID}
			if browserWindow != nil {
				loc.browser = appID
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t\n", formatLocation(loc), workspaceName, visible, appID, window.Name)

			for _, t := range browserWindow {
				tabVisible := ""
				if visible != "" && t.id == activeTab {
					tabVisible = "1"
				}
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", formatLocation(location{con: window.ID, browser: loc.browser, tab: t.id}), workspaceName, tabVisible, appID, t.title, t.url)
			}
		}
	}

	for appID, unmatched := range tabs {
		for _, browserTabs := range unmatched {
			for _, t := range browserTabs {
				fmt.Fprintf(out, "%s\t\t\t%s\t%s\t%s\n", formatLocation(location{browser: appID, tab: t.id}), appID, t.title, t.url)
			}
		}
	}

	return nil
}

// a browser window is titled after its active tab, with the browser's own suffix
// appended, so require the match to end on a word boundary
func titleMatches(windowTitle, tabTitle string) bool {
	return windowTitle == tabTitle || strings.HasPrefix(windowTitle, tabTitle+" ")
}

func iterWorkspaces(root *sway.Node) iter.Seq[*sway.Node] {
	return func(yield func(*sway.Node) bool) {
		for _, output := range root.Nodes {
			for _, workspace := range output.Nodes {
				if workspace.Type != sway.NodeWorkspace {
					continue
				}
				if !yield(workspace) {
					return
				}
			}
		}
	}
}

func findWindows(node *sway.Node) []*sway.Node {
	var windows []*sway.Node
	if node.PID != nil {
		windows = append(windows, node)
	}
	for _, node := range node.Nodes {
		windows = append(windows, findWindows(node)...)
	}
	for _, node := range node.FloatingNodes {
		windows = append(windows, findWindows(node)...)
	}
	return windows
}

type handler struct {
	sway.EventHandler
	browsers browsers
	history  []location
}

const historySize = 10

func (h *handler) Window(ctx context.Context, e sway.WindowEvent) {
	switch e.Change {
	case sway.WindowFocus, sway.WindowTitle:
		if e.Change == sway.WindowTitle && !e.Container.Focused {
			return
		}

		current := location{con: e.Container.ID}
		if appID := e.Container.AppID; appID != nil {
			if browser, ok := h.browsers[*appID]; ok {
				current.browser, current.tab = *appID, browser.activeTab(e.Container.Name)
			}
		}
		if len(h.history) > 0 {
			top := h.history[len(h.history)-1]
			if top == current || (top.con == current.con && current.tab == "") {
				return
			}
			if top.con == current.con && top.tab == "" {
				h.history = h.history[:len(h.history)-1]
			}
		}

		h.history = append(slices.DeleteFunc(h.history, func(l location) bool { return l == current }), current)
		if len(h.history) > historySize {
			h.history = h.history[1:]
		}

	case sway.WindowClose:
		h.history = slices.DeleteFunc(h.history, func(l location) bool { return l.con == e.Container.ID })

	default:
		return
	}

	if err := writeBack(h.history); err != nil {
		fmt.Fprintf(os.Stderr, "error writing back location: %v\n", err)
	}
}

func writeBack(history []location) error {
	if len(history) < 2 {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	previous := history[len(history)-2]
	return os.WriteFile(statePath, []byte(formatLocation(previous)+"\n"), 0o600)
}

func retry(ctx context.Context, name string, f func() error) error {
	for {
		if err := f(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if _, err := os.Stat(os.Getenv("SWAYSOCK")); err != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "%s error, retrying: %v\n", name, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(1 * time.Second):
			}
			continue
		}
		return nil
	}
}

// bruvtabBrowser talks to a bruvtab mediator, where a tab id is <browser window>.<tab>
type bruvtabBrowser struct {
	url *url.URL
}

func bruvtabParseBrowser(value string) (browser, error) {
	url, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	return bruvtabBrowser{url: url}, nil
}

func (b bruvtabBrowser) windows() [][]tab {
	body, err := bruvtabGet(b.url, nil, "list_tabs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listing tabs: %v\n", err)
		return nil
	}

	var windows [][]tab
	var indexes = map[string]int{}
	for _, t := range bruvtabParseTabs(body) {
		window, _, _ := strings.Cut(t.id, ".")
		index, ok := indexes[window]
		if !ok {
			index = len(windows)
			indexes[window] = index
			windows = append(windows, nil)
		}
		windows[index] = append(windows[index], t)
	}
	return windows
}

func (b bruvtabBrowser) activeTab(windowTitle string) string {
	query := base64.URLEncoding.EncodeToString([]byte(`{"active":true}`))
	body, err := bruvtabGet(b.url, nil, "query_tabs", query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error querying active tabs: %v\n", err)
		return ""
	}

	var found, foundTitle string
	var ambiguous bool
	for _, t := range bruvtabParseTabs(body) {
		if !titleMatches(windowTitle, t.title) {
			continue
		}
		switch {
		case len(t.title) > len(foundTitle):
			found, foundTitle, ambiguous = t.id, t.title, false
		case len(t.title) == len(foundTitle):
			ambiguous = true // better unknown than wrong
		}
	}
	if ambiguous {
		return ""
	}
	return found
}

func (b bruvtabBrowser) focus(tabID string) error {
	_, tab, _ := strings.Cut(tabID, ".")
	_, err := bruvtabGet(b.url, url.Values{"focused": {"1"}}, "activate_tab", tab)
	return err
}

func bruvtabParseTabs(body string) []tab {
	var tabs []tab
	for line := range strings.Lines(body) {
		id, rest, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		title, url, _ := strings.Cut(rest, "\t")
		if title == "" {
			continue
		}
		tabs = append(tabs, tab{id: id, title: title, url: url})
	}
	return tabs
}

var bruvtabClient = &http.Client{Timeout: 1 * time.Second}

func bruvtabGet(bruvtab *url.URL, query url.Values, path ...string) (string, error) {
	request := bruvtab.JoinPath(path...)
	request.RawQuery = query.Encode()

	resp, err := bruvtabClient.Get(request.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
