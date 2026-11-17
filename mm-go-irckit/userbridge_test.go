package irckit

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

const (
	testBridgeUser     = "testalice"
	testBridgeTeam     = "testteam"
	testBridgeTeamName = "testteamname"
	testPendingChan    = "ch1"
)

type nopNetConn struct{}

func (nopNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopNetConn) Write(p []byte) (int, error)      { return len(p), nil }
func (nopNetConn) Close() error                     { return nil }
func (nopNetConn) LocalAddr() net.Addr              { return nil }
func (nopNetConn) RemoteAddr() net.Addr             { return nil }
func (nopNetConn) SetDeadline(time.Time) error      { return nil }
func (nopNetConn) SetReadDeadline(time.Time) error  { return nil }
func (nopNetConn) SetWriteDeadline(time.Time) error { return nil }

type replayTestBridge struct {
	bridge.Bridger

	protocol        string
	cfg             *config.Config
	channels        []*bridge.ChannelInfo
	getChannelUsers func(ctx context.Context, channelID string) ([]*bridge.UserInfo, error)
	getReplayEvents func(ctx context.Context, channelID string, since int64) []*bridge.Event

	mu          sync.Mutex
	viewedCalls []string
}

func (b *replayTestBridge) Protocol() string {
	return b.protocol
}

func (b *replayTestBridge) BridgeConfig() *config.BridgeConfig {
	if b.protocol == serviceMatrix {
		return &b.cfg.Current().Matrix.Bridge
	}

	return &b.cfg.Current().Mattermost.Bridge
}

func (b *replayTestBridge) FormatterConfig() *config.FormatterConfig {
	return &config.FormatterConfig{}
}

func (b *replayTestBridge) GetMe() *bridge.UserInfo {
	return &bridge.UserInfo{User: testBridgeUser, Nick: testBridgeUser, TeamID: testBridgeTeam}
}

func (b *replayTestBridge) GetUsers() []*bridge.UserInfo {
	return nil
}

func (b *replayTestBridge) GetChannels() []*bridge.ChannelInfo {
	return b.channels
}

func (b *replayTestBridge) GetChannel(_ context.Context, channelID string) (*bridge.ChannelInfo, error) {
	return &bridge.ChannelInfo{ID: channelID, Name: channelID, TeamID: testBridgeTeam}, nil
}

func (b *replayTestBridge) GetChannelName(_ context.Context, channelID string) string {
	return "#" + channelID
}

func (b *replayTestBridge) GetTeamName(_ context.Context, _ string) string {
	return testBridgeTeamName
}

func (b *replayTestBridge) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	if b.getChannelUsers != nil {
		return b.getChannelUsers(ctx, channelID)
	}

	return nil, nil
}

func (b *replayTestBridge) IsChannelMember(_ string) bool {
	return true
}

func (b *replayTestBridge) IsDMChannelName(_ string) bool {
	return false
}

func (b *replayTestBridge) Topic(_ context.Context, _ string) string {
	return ""
}

func (b *replayTestBridge) GetLastViewedAt(_ context.Context, _ string) int64 {
	return 0
}

func (b *replayTestBridge) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	if b.getReplayEvents != nil {
		return b.getReplayEvents(ctx, channelID, since)
	}

	return []*bridge.Event{}
}

func (b *replayTestBridge) UpdateLastViewed(_ context.Context, channelID string) {
	b.mu.Lock()
	b.viewedCalls = append(b.viewedCalls, channelID)
	b.mu.Unlock()
}

func (b *replayTestBridge) Logout(_ context.Context) error {
	return nil
}

func (b *replayTestBridge) viewedCount(channelID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	count := 0

	for _, id := range b.viewedCalls {
		if id == channelID {
			count++
		}
	}

	return count
}

func newTestUserBridge(t *testing.T, tomlContent string, br *replayTestBridge) (*User, *bolt.DB) {
	t.Helper()

	prevLogger := logger

	l := logrus.New()
	l.SetOutput(io.Discard)
	SetLogger(logrus.NewEntry(l))
	t.Cleanup(func() {
		SetLogger(prevLogger)
	})

	cfgPath := filepath.Join(t.TempDir(), "matterircd.toml")

	err := os.WriteFile(cfgPath, []byte(tomlContent), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatal(err)
	}

	br.cfg = cfg

	dbPath := filepath.Join(t.TempDir(), "lastviewed.db")

	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	srv := NewServer("matterircd")
	u := NewUserBridge(nopNetConn{}, srv, cfg, map[string]*bolt.DB{"default": db})
	u.User = testBridgeUser
	u.Nick = testBridgeUser
	u.Me = true
	u.br = br
	u.loginReady.Store(true)

	t.Cleanup(func() {
		if u.cancel != nil {
			u.cancel()
		}

		u.stopEventLoop()
	})

	return u, db
}

func readSavedLastViewed(t *testing.T, db *bolt.DB, u *User, channelID string) (int64, bool) {
	t.Helper()

	var (
		ts    int64
		found bool
	)

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(u.dbBucketName())
		if b == nil {
			return nil
		}

		v := b.Get([]byte(channelID))
		if len(v) != 8 {
			return nil
		}

		found = true

		for i := range 8 {
			ts |= int64(v[i]) << (8 * i)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return ts, found
}

func TestPendingReplayGuards(t *testing.T) {
	prevDelay := lastViewedMaxDelay
	lastViewedMaxDelay = 25 * time.Millisecond

	t.Cleanup(func() {
		lastViewedMaxDelay = prevDelay
	})

	br := &replayTestBridge{protocol: serviceMattermost}
	u, db := newTestUserBridge(t, "", br)

	const (
		earlierTS = int64(1000)
		newerTS   = int64(5000)
	)

	u.saveLastViewedAt(testPendingChan, earlierTS)

	got, ok := readSavedLastViewed(t, db, u, testPendingChan)
	assert.True(t, ok)
	assert.Equal(t, earlierTS, got)

	assert.False(t, u.isPendingReplay(""))
	u.clearPendingReplay("")
	u.markPendingReplay([]*bridge.ChannelInfo{nil, {ID: ""}})
	assert.False(t, u.isPendingReplay(testPendingChan))

	// Trigger updateLastViewed before marking pending, then mark pending during its sleep window.
	u.updateLastViewed(testPendingChan)
	u.markPendingReplay([]*bridge.ChannelInfo{{ID: testPendingChan}})
	assert.True(t, u.isPendingReplay(testPendingChan))

	time.Sleep(40 * time.Millisecond)
	assert.Equal(t, 0, br.viewedCount(testPendingChan))

	u.saveLastViewedAt(testPendingChan, newerTS)

	got, ok = readSavedLastViewed(t, db, u, testPendingChan)
	assert.True(t, ok)
	assert.Equal(t, earlierTS, got)

	// Unrelated channels remain writable while testPendingChan is pending.
	u.saveLastViewedAt("ch2", newerTS)

	got2, ok2 := readSavedLastViewed(t, db, u, "ch2")
	assert.True(t, ok2)
	assert.Equal(t, newerTS, got2)
}

func TestPendingReplayRefcount(t *testing.T) {
	br := &replayTestBridge{protocol: serviceMattermost}
	u, db := newTestUserBridge(t, "", br)

	const (
		earlierTS = int64(1000)
		newerTS   = int64(5000)
	)

	u.saveLastViewedAt(testPendingChan, earlierTS)

	// Overlapping mark increments the refcount so one clear keeps the channel protected.
	u.markPendingReplay([]*bridge.ChannelInfo{{ID: testPendingChan}})
	u.markPendingReplay([]*bridge.ChannelInfo{{ID: testPendingChan}})
	u.clearPendingReplay(testPendingChan)
	assert.True(t, u.isPendingReplay(testPendingChan))

	u.saveLastViewedAt(testPendingChan, newerTS)

	got, ok := readSavedLastViewed(t, db, u, testPendingChan)
	assert.True(t, ok)
	assert.Equal(t, earlierTS, got)

	// Second clear drops protection and allows cursor updates again.
	u.clearPendingReplay(testPendingChan)
	assert.False(t, u.isPendingReplay(testPendingChan))

	u.saveLastViewedAt(testPendingChan, newerTS)

	got, ok = readSavedLastViewed(t, db, u, testPendingChan)
	assert.True(t, ok)
	assert.Equal(t, newerTS, got)
}

func runWorkerForChannels(u *User, channels []*bridge.ChannelInfo, beforeWorker func()) {
	u.markPendingReplay(channels)

	if beforeWorker != nil {
		beforeWorker()
	}

	chQueue := make(chan *bridge.ChannelInfo, len(channels))
	for _, ch := range channels {
		chQueue <- ch
	}

	close(chQueue)

	throttle := time.NewTicker(time.Millisecond)
	defer throttle.Stop()

	u.addUserToChannelWorker(chQueue, throttle, logger)
}

func TestAddUserToChannelWorkerPendingReplaySuccess(t *testing.T) {
	const chOK = "ch-ok"

	prevDelay := lastViewedMaxDelay
	lastViewedMaxDelay = time.Millisecond

	t.Cleanup(func() {
		lastViewedMaxDelay = prevDelay
	})

	seededTS := time.Now().Add(-2 * time.Hour).UnixMilli()
	liveTS := time.Now().Add(-time.Minute).UnixMilli()

	var (
		mu          sync.Mutex
		replaySince = make(map[string]int64)
		u           *User
		db          *bolt.DB
	)

	br := &replayTestBridge{
		protocol: serviceMatrix,
		getReplayEvents: func(_ context.Context, channelID string, since int64) []*bridge.Event {
			mu.Lock()
			replaySince[channelID] = since
			mu.Unlock()

			return []*bridge.Event{}
		},
	}

	u, db = newTestUserBridge(t, "[matrix]\nDisableAutoView = false\n", br)
	u.saveLastViewedAt(chOK, seededTS)

	runWorkerForChannels(u, []*bridge.ChannelInfo{
		{ID: chOK, Name: "ok", TeamID: testBridgeTeam, LastPostAt: liveTS},
	}, func() {
		// Simulate live traffic arriving while chOK is queued ahead of its worker.
		u.saveLastViewedAt(chOK, liveTS)
		u.updateLastViewed(chOK)
	})

	mu.Lock()
	assert.Equal(t, seededTS, replaySince[chOK])
	mu.Unlock()
	assert.False(t, u.isPendingReplay(chOK))

	tsOK, ok := readSavedLastViewed(t, db, u, chOK)
	assert.True(t, ok)
	assert.Greater(t, tsOK, liveTS)

	assert.Eventually(t, func() bool {
		return br.viewedCount(chOK) == 1
	}, time.Second, 5*time.Millisecond)
}

func TestAddUserToChannelWorkerPendingReplaySkipsAndFailure(t *testing.T) {
	const (
		chLazySkip   = "ch-lazy-skip"
		chReplayFail = "ch-replay-fail"
	)

	tomlCfg := "[matrix]\nEnableLazyJoin = true\nLazyJoinDuration = \"24h\"\nDisableAutoView = false\n"
	seededTS := time.Now().Add(-2 * time.Hour).UnixMilli()
	liveTS := time.Now().Add(-time.Minute).UnixMilli()
	oldTS := time.Now().Add(-72 * time.Hour).UnixMilli()

	var (
		u  *User
		db *bolt.DB
	)

	br := &replayTestBridge{
		protocol: serviceMatrix,
		getChannelUsers: func(_ context.Context, channelID string) ([]*bridge.UserInfo, error) {
			if channelID == chReplayFail {
				u.saveLastViewedAt(chReplayFail, liveTS)
				u.updateLastViewed(chReplayFail)
			}

			return nil, nil
		},
		getReplayEvents: func(_ context.Context, _ string, _ int64) []*bridge.Event {
			return nil
		},
	}

	u, db = newTestUserBridge(t, tomlCfg, br)
	u.saveLastViewedAt(chLazySkip, seededTS)
	u.saveLastViewedAt(chReplayFail, seededTS)

	runWorkerForChannels(u, []*bridge.ChannelInfo{
		{ID: chLazySkip, Name: "lazy", TeamID: testBridgeTeam, LastPostAt: oldTS},
		{ID: chReplayFail, Name: "fail", TeamID: testBridgeTeam, LastPostAt: liveTS},
	}, nil)

	// LazyJoin skip clears pendingReplay without overwriting BoltDB and re-enables future saves.
	assert.False(t, u.isPendingReplay(chLazySkip))

	tsLazy, ok := readSavedLastViewed(t, db, u, chLazySkip)
	assert.True(t, ok)
	assert.Equal(t, seededTS, tsLazy)

	u.saveLastViewedAt(chLazySkip, liveTS)

	tsLazy, _ = readSavedLastViewed(t, db, u, chLazySkip)
	assert.Equal(t, liveTS, tsLazy)

	// Failed replay clears pendingReplay while keeping the original cursor and skipping UpdateLastViewed.
	assert.False(t, u.isPendingReplay(chReplayFail))

	tsFail, ok := readSavedLastViewed(t, db, u, chReplayFail)
	assert.True(t, ok)
	assert.Equal(t, seededTS, tsFail)
	assert.Equal(t, 0, br.viewedCount(chReplayFail))
}

func TestAddUserToChannelWorkerCancelClearsPending(t *testing.T) {
	entered := make(chan struct{})

	br := &replayTestBridge{
		protocol: serviceMatrix,
		getChannelUsers: func(ctx context.Context, _ string) ([]*bridge.UserInfo, error) {
			close(entered)
			<-ctx.Done()

			return nil, ctx.Err()
		},
	}

	u, _ := newTestUserBridge(t, "", br)

	cancelCh := []*bridge.ChannelInfo{{
		ID:         "ch-cancel",
		Name:       "cancel",
		TeamID:     testBridgeTeam,
		LastPostAt: time.Now().UnixMilli(),
	}}
	u.markPendingReplay(cancelCh)

	cancelQueue := make(chan *bridge.ChannelInfo, 1)
	cancelQueue <- cancelCh[0]

	close(cancelQueue)

	throttle := time.NewTicker(time.Millisecond)
	defer throttle.Stop()

	done := make(chan struct{})

	go func() {
		defer close(done)

		u.addUserToChannelWorker(cancelQueue, throttle, logger)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter GetChannelUsers")
	}

	u.cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after cancel")
	}

	assert.False(t, u.isPendingReplay("ch-cancel"))
}

func TestAddUsersToChannelsDrainsPendingOnCancel(t *testing.T) {
	entered := make(chan struct{})

	var enteredOnce sync.Once

	nowMs := time.Now().UnixMilli()
	chans := []*bridge.ChannelInfo{
		{ID: "ch-a", Name: "a", TeamID: testBridgeTeam, LastPostAt: nowMs},
		{ID: "ch-b", Name: "b", TeamID: testBridgeTeam, LastPostAt: nowMs},
		{ID: "ch-c", Name: "c", TeamID: testBridgeTeam, LastPostAt: nowMs},
		{ID: "ch-d", Name: "d", TeamID: testBridgeTeam, LastPostAt: nowMs},
		{ID: "ch-e", Name: "e", TeamID: testBridgeTeam, LastPostAt: nowMs},
		{ID: "ch-f", Name: "f", TeamID: testBridgeTeam, LastPostAt: nowMs},
	}

	br := &replayTestBridge{
		protocol: serviceMattermost,
		channels: chans,
		getChannelUsers: func(ctx context.Context, _ string) ([]*bridge.UserInfo, error) {
			enteredOnce.Do(func() {
				close(entered)
			})
			<-ctx.Done()

			return nil, ctx.Err()
		},
	}

	u, _ := newTestUserBridge(t, "", br)

	go u.addUsersToChannels()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for addUsersToChannels worker to start")
	}

	// Workers block on GetChannelUsers while remaining entries in channels drain after wg.Wait.
	u.cancel()

	assert.Eventually(t, func() bool {
		for _, ch := range chans {
			if u.isPendingReplay(ch.ID) {
				return false
			}

			if _, ok := u.getSavedSinceSnap(ch.ID); ok {
				return false
			}
		}

		return true
	}, 2*time.Second, 10*time.Millisecond)
}

func writeRawLastViewed(t *testing.T, db *bolt.DB, bucket []byte, channelID string, ts int64) {
	t.Helper()

	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}

		buf := make([]byte, 8)

		//nolint:gosec // test timestamps are non-negative UnixMilli values
		binary.LittleEndian.PutUint64(buf, uint64(ts))

		return b.Put([]byte(channelID), buf)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSavedSinceSnapshotPinsPreSyncCursor(t *testing.T) {
	const (
		chSeeded = "ch-snap-seeded"
		chAbsent = "ch-snap-absent"
		chLegacy = "ch-snap-legacy"
	)

	br := &replayTestBridge{protocol: serviceMattermost}
	u, db := newTestUserBridge(t, "[mattermost]\nReplayStrategy = \"saved\"\n", br)

	seededTS := time.Now().Add(-2 * time.Hour).UnixMilli()
	laterTS := time.Now().Add(-time.Minute).UnixMilli()
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()

	u.saveLastViewedAt(chSeeded, seededTS)
	writeRawLastViewed(t, db, []byte(u.User), chLegacy, seededTS)

	channels := []*bridge.ChannelInfo{{ID: chSeeded}, {ID: chAbsent}, {ID: chLegacy}}
	u.markPendingReplay(channels)

	// Bypass saveLastViewedAt to verify getChannelSince reads the upfront BoltDB snapshot.
	writeRawLastViewed(t, db, u.dbBucketName(), chSeeded, laterTS)
	writeRawLastViewed(t, db, u.dbBucketName(), chAbsent, laterTS)
	writeRawLastViewed(t, db, []byte(u.User), chLegacy, laterTS)

	since, src, inDB := u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chSeeded, Name: "seeded"}, cutoff)
	assert.Equal(t, seededTS, since)
	assert.Equal(t, "stored", src)
	assert.True(t, inDB)

	since, src, inDB = u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chAbsent, Name: "absent"}, cutoff)
	assert.Equal(t, cutoff, since)
	assert.Equal(t, "cutoff-fallback", src)
	assert.False(t, inDB)

	since, _, inDB = u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chLegacy, Name: "legacy"}, cutoff)
	assert.Equal(t, seededTS, since)
	assert.True(t, inDB)

	for _, ch := range channels {
		u.clearPendingReplay(ch.ID)
		_, ok := u.getSavedSinceSnap(ch.ID)
		assert.False(t, ok)
	}

	since, _, inDB = u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chSeeded, Name: "seeded"}, cutoff)
	assert.Equal(t, laterTS, since)
	assert.True(t, inDB)
}

func TestSavedSinceSnapshotRefcountAndServerOnly(t *testing.T) {
	const chRef = "ch-snap-ref"

	br := &replayTestBridge{protocol: serviceMattermost}
	u, db := newTestUserBridge(t, "[mattermost]\nReplayStrategy = \"hybrid\"\n", br)

	seededTS := time.Now().Add(-2 * time.Hour).UnixMilli()
	laterTS := time.Now().Add(-time.Minute).UnixMilli()
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()

	u.saveLastViewedAt(chRef, seededTS)

	u.markPendingReplay([]*bridge.ChannelInfo{{ID: chRef}})
	u.markPendingReplay([]*bridge.ChannelInfo{{ID: chRef}})
	writeRawLastViewed(t, db, u.dbBucketName(), chRef, laterTS)

	u.clearPendingReplay(chRef)

	since, _, inDB := u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chRef, Name: "ref"}, cutoff)
	assert.Equal(t, seededTS, since)
	assert.True(t, inDB)

	u.clearPendingReplay(chRef)

	since, _, inDB = u.getChannelSince(context.Background(), &bridge.ChannelInfo{ID: chRef, Name: "ref"}, cutoff)
	assert.Equal(t, laterTS, since)
	assert.True(t, inDB)

	uServerOnly, _ := newTestUserBridge(t, "[mattermost]\nReplayStrategy = \"server-only\"\n", &replayTestBridge{protocol: serviceMattermost})
	uServerOnly.markPendingReplay([]*bridge.ChannelInfo{{ID: chRef}})
	_, ok := uServerOnly.getSavedSinceSnap(chRef)
	assert.False(t, ok)
	uServerOnly.clearPendingReplay(chRef)
}
