// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-github/v31/github"
	"golang.org/x/oauth2"
	"k8s.io/klog/v2"

	"github.com/google/triage-party/pkg/initcache"
	"github.com/google/triage-party/pkg/site"
	"github.com/google/triage-party/pkg/triage"
	"github.com/google/triage-party/pkg/updater"
)

var (
	// shared with tester
	configPath      = flag.String("config", "", "configuration path")
	initCachePath   = flag.String("initcache", "", "Where to load the initial cache from (optional)")
	reposOverride   = flag.String("repos", "", "Override configured repos with this repository (comma separated)")
	githubTokenFile = flag.String("github-token-file", "", "github token secret file, also settable via GITHUB_TOKEN")

	// server specific
	siteDir       = flag.String("site", "site/", "path to site files")
	thirdPartyDir = flag.String("3p", "third_party/", "path to 3rd party files")
	dryRun        = flag.Bool("dry-run", false, "run queries, don't start a server")
	port          = flag.Int("port", 8080, "port to run server at")
	siteName      = flag.String("name", "", "override site name from config file")

	maxRefresh    = flag.Duration("max-refresh", 60*time.Minute, "Maximum time between collection runs")
	minRefresh    = flag.Duration("min-refresh", 60*time.Second, "Minimum time between collection runs")
	memberRefresh = flag.Duration("membership-refresh", 24*time.Hour, "Minimum time between refreshing membership information")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	if *configPath == "" {
		klog.Exitf("--config is required")
	}

	ctx := context.Background()

	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: triage.MustReadToken(*githubTokenFile, "GITHUB_TOKEN")},
	)))

	f, err := os.Open(findPath(*configPath))
	if err != nil {
		klog.Exitf("open %s: %v", *configPath, err)
	}

	cachePath := *initCachePath
	if cachePath == "" {
		cachePath = initcache.DefaultDiskPath(*configPath, *reposOverride)
	}
	klog.Infof("cache path: %s", cachePath)

	c := initcache.New(initcache.Config{Type: "disk", Path: cachePath})
	if err := c.Initialize(); err != nil {
		klog.Exitf("initcache load to %s: %v", cachePath, err)
	}

	cfg := triage.Config{
		Client:        client,
		Cache:         c,
		MemberRefresh: *memberRefresh,
	}

	if *reposOverride != "" {
		cfg.Repos = strings.Split(*reposOverride, ",")
	}

	tp := triage.New(cfg)
	if err := tp.Load(f); err != nil {
		klog.Exitf("load from %s: %v", *configPath, err)
	}

	ts, err := tp.ListRules()
	if err != nil {
		klog.Exitf("list rules: %v", err)
	}

	klog.Infof("Loaded %d rules", len(ts))
	sn := *siteName
	if sn == "" {
		sn = calculateSiteName(ts)
	}

	// Make sure save works
	if err := c.Save(); err != nil {
		klog.Exitf("initcache save to %s: %v", cachePath, err)
	}

	u := updater.New(updater.Config{
		Party:      tp,
		MinRefresh: *minRefresh,
		MaxRefresh: *maxRefresh,
		PersistFunc: func() error {
			return c.Save()
		},
	})

	if *dryRun {
		klog.Infof("Updating ...")
		if err := u.RunOnce(ctx, true); err != nil {
			klog.Exitf("run failed: %v", err)
		}
		os.Exit(0)
	}

	klog.Infof("Starting update loop: %+v", u)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range sigc {
			klog.Infof("signal caught: %v", sig)
			if err := c.Save(); err != nil {
				klog.Errorf("save errro: %v", err)
			}
			os.Exit(0)
		}
	}()

	go func() {
		if err := u.Loop(ctx); err == nil {
			klog.Exitf("loop failed: %v", err)
		}
	}()

	s := site.New(&site.Config{
		BaseDirectory: findPath(*siteDir),
		Updater:       u,
		Party:         tp,
		WarnAge:       2 * *maxRefresh,
		Name:          sn,
	})

	http.Handle("/third_party/", http.StripPrefix("/third_party/", http.FileServer(http.Dir(findPath(*thirdPartyDir)))))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join(findPath(*siteDir), "static")))))
	http.HandleFunc("/s/", s.Collection())
	http.HandleFunc("/", s.Root())

	listenAddr := fmt.Sprintf(":%s", os.Getenv("PORT"))
	if listenAddr == ":" {
		listenAddr = fmt.Sprintf(":%d", *port)
	}

	fmt.Printf("\n\n*** teaparty is listening at %s ... ***\n\n", listenAddr)
	err = http.ListenAndServe(listenAddr, nil)
	if err != nil {
		panic(err)
	}
}

// calculates a user-friendly site name based on repositories
func calculateSiteName(ts []triage.Rule) string {
	seen := map[string]bool{}
	for _, t := range ts {
		for _, r := range t.Repos {
			parts := strings.Split(r, "/")
			seen[parts[len(parts)-1]] = true
		}
	}

	names := []string{}
	for n := range seen {
		names = append(names, n)
	}
	return strings.Join(names, " + ")
}

// findPath tries to find the right place for a file
func findPath(p string) string {
	// Running from triage-party/
	if _, err := os.Stat(p); err == nil {
		return p
	}

	// Running from triage-party/cmd/server
	wd, err := os.Getwd()
	if err != nil {
		klog.Errorf("crazy: %v", err)
		return p
	}
	if filepath.Base(wd) == "server" {
		tp := "../../" + p
		if _, err := os.Stat(tp); err == nil {
			return tp
		}
	}
	return p
}
