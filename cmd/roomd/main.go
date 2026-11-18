// Command roomd orchestrates multi-bot Matrix group rooms backed by Hermes
// Agent profiles. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ziliding/hermes-roomd/internal/config"
	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/hindsight"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/media"
	"github.com/ziliding/hermes-roomd/internal/memory"
	"github.com/ziliding/hermes-roomd/internal/room"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/router"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

func main() {
	cfgPath := flag.String("config", "roomd.yaml", "path to the YAML config")
	debug := flag.Bool("debug", false, "debug logging")
	check := flag.Bool("check", false, "only load and validate the config (and its rules files and env vars), then exit")
	flag.Parse()
	if *check {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// media_dirs are checked when roomd starts, on the host that has them.
		fmt.Printf("config ok: %d room(s), %d bot(s), router=%v, memory=%v\n", len(cfg.Rooms), len(cfg.Bots), cfg.Router.Enabled, cfg.Memory.Enabled)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *cfgPath, log); err != nil {
		log.Error("roomd stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfgPath string, log *slog.Logger) error {
	startAt := time.Now()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	files, err := media.NewFiles(cfg.MediaDirs, media.MaxFileBytes)
	if err != nil {
		return fmt.Errorf("media_dirs: %w", err)
	}
	store, err := transcript.Open(ctx, cfg.StateDB)
	if err != nil {
		return err
	}
	defer store.Close()

	keys := cfg.BotKeys()
	hermesFor := map[string]*hermes.Client{}
	senders := map[string]*matrixio.Sender{}
	for _, k := range keys {
		b := cfg.Bots[k]
		s, err := matrixio.NewSender(ctx, cfg.Homeserver, b.MXID, b.AccessToken, string(cfg.MsgType))
		if err != nil {
			return fmt.Errorf("bot %s: %w", k, err)
		}
		senders[k] = s
		hermesFor[k] = hermes.New(b.Hermes.BaseURL, b.Hermes.Key, b.Hermes.Model)
	}

	// One /sync loop per listener bot, for the rooms that bot listens to.
	groups := cfg.ListenerGroups()
	for listener, rooms := range groups {
		if err := matrixio.CheckRooms(ctx, senders[listener].Client(), rooms); err != nil {
			return err
		}
	}
	if cfg.Router.Enabled {
		if missing := cfg.MissingBlurbs(); len(missing) > 0 {
			log.Warn("router: these bots have no real blurb, so the router cannot tell them apart; write one line about each character in roomd.yaml",
				"bots", strings.Join(missing, ","))
		}
	}

	posters := map[string]room.Poster{}
	runners := map[string]room.Runner{}
	for _, k := range keys {
		posters[k], runners[k] = senders[k], hermesFor[k]
	}
	var routerLog io.Writer
	if cfg.Router.Enabled && cfg.Router.LogFile != "" {
		f, err := os.OpenFile(cfg.Router.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("router.log_file: %w", err)
		}
		defer f.Close()
		routerLog = f
	}
	specs, retainers := buildRooms(cfg, store, posters, runners, routerLog, uint64(time.Now().UnixNano()), log)
	orch := room.New(room.Options{Store: store, RoomSpecs: specs, MaxDeltaLines: cfg.MaxDeltaLines, Log: log, RunTimeout: cfg.HermesTimeout, Files: files})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		orch.Run(ctx)
		close(done)
	}()
	retainDone := make(chan struct{})
	go func() {
		defer close(retainDone)
		memory.RunRetainers(ctx, time.Minute, retainers...) // first pass now: catches up after a restart
	}()
	log.Info("roomd ready", "rooms", len(cfg.Rooms), "bots", len(keys), "sync_loops", len(groups),
		"router", cfg.Router.Enabled, "router_model", cfg.Router.Model,
		"memory", cfg.Memory.Enabled, "retainers", len(retainers), "media_dirs", len(cfg.MediaDirs))

	// Any sync loop failing stops roomd (systemd restarts it).
	errc := make(chan error, len(groups))
	for listener, rooms := range groups {
		go func() {
			err := matrixio.Listen(ctx, senders[listener].Client(), rooms, startAt, log.With("listener", listener), func(m matrixio.Message) {
				orch.Handle(ctx, m)
			})
			if err != nil {
				err = fmt.Errorf("matrix sync as %s: %w", listener, err)
			}
			errc <- err
			cancel()
		}()
	}
	var firstErr error
	for range groups {
		if err := <-errc; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	cancel()
	<-done
	<-retainDone
	return firstErr
}

// buildRooms builds each room's cast, rules, turn engine and memory from the
// config: one roster per room (the user plus that room's bots), the room's
// rules rendered per bot, a decider over that cast with the room's
// max_bot_turns, and recall/retain against the room's own bank.
func buildRooms(cfg *config.Config, store *transcript.Store, posters map[string]room.Poster, runners map[string]room.Runner,
	routerLog io.Writer, seed uint64, log *slog.Logger) ([]room.RoomSpec, []*memory.Retainer) {
	blurbs := map[string]string{}
	for k, b := range cfg.Bots {
		blurbs[k] = b.Blurb
	}
	banks := map[string]*hindsight.Client{} // one client per bank id
	var specs []room.RoomSpec
	var retainers []*memory.Retainer
	for i, rc := range cfg.Rooms {
		var people []roster.Person
		for _, k := range rc.Bots {
			b := cfg.Bots[k]
			people = append(people, roster.Person{Key: k, Display: b.Display, MXID: b.MXID})
		}
		r := roster.New(cfg.User, cfg.UserDisplay, people)
		var bots []*room.Bot
		for _, k := range rc.Bots {
			bots = append(bots, &room.Bot{
				Key: k, Poster: posters[k], Hermes: runners[k],
				System: room.SystemPrompt(rc.Rules, r, k, blurbs), // per room: the room's rules and cast
			})
		}
		s := seed + uint64(i)
		var decider turns.Decider = turns.NewRules(rc.Bots, rc.MaxBotTurns, rand.New(rand.NewPCG(s, s^0x9e3779b97f4a7c15)))
		if c := cfg.Router; c.Enabled {
			decider = router.New(router.Options{
				BaseURL: c.BaseURL, APIKey: c.Key, Model: c.Model, Timeout: c.Timeout, Window: c.Window, JSONMode: *c.JSONMode,
				Transcript: store, Roster: r, Blurbs: blurbs, MaxBotTurns: rc.MaxBotTurns,
				Fallback: decider, // the turn rules decide whenever the router call fails
				Log:      log, CallLog: routerLog,
			})
		}
		spec := room.RoomSpec{ID: rc.ID, Roster: r, Decider: decider, Group: roster.NewGroupMatcher(cfg.GroupWords),
			Bots: bots, Notifier: rc.Listener,
			BackgroundAfter: time.Duration(*rc.BackgroundAfter)}

		if mc := cfg.Memory; mc.Enabled {
			eff := cfg.MemoryFor(rc)
			hs := banks[eff.BankID]
			if hs == nil {
				hs = hindsight.New(mc.APIURL, eff.BankID, mc.APIKey)
				banks[eff.BankID] = hs
			}
			if eff.Recall {
				spec.Memory = memory.NewRecall(memory.RecallOptions{
					Client: hs, Transcript: store, Roster: r,
					MaxTokens: mc.Recall.MaxTokens, Budget: hindsight.Budget(mc.Recall.Budget),
					Timeout: mc.Recall.Timeout, MinScore: *mc.Recall.MinScore, Log: log,
				})
			}
			if eff.Retain {
				var sum memory.Summarizer
				if mc.Retain.Mode == config.RetainSummary {
					sum = &memory.ChatSummarizer{
						BaseURL: cfg.Router.BaseURL, APIKey: cfg.Router.Key, Model: cfg.Router.Model,
						System: memory.SummaryPrompt(r),
					}
				}
				retainers = append(retainers, memory.NewRetainer(memory.RetainOptions{
					Store: store, Writer: hs, Summarizer: sum, Roster: r, Rooms: []string{rc.ID},
					Mode: memory.Mode(mc.Retain.Mode), Idle: time.Duration(eff.IdleMinutes) * time.Minute,
					MinMessages: eff.MinMessages, Timeout: mc.Retain.Timeout, Log: log,
				}))
			}
			log.Info("room memory", "room", rc.Label(), "bank", eff.BankID, "recall", eff.Recall, "retain", eff.Retain)
		}
		specs = append(specs, spec)
		log.Info("room", "room", rc.Label(), "id", rc.ID, "bots", strings.Join(rc.Bots, ","), "listener", rc.Listener,
			"max_bot_turns", rc.MaxBotTurns, "background_after", time.Duration(*rc.BackgroundAfter), "custom_rules", rc.RulesFile != "")
	}
	return specs, retainers
}
