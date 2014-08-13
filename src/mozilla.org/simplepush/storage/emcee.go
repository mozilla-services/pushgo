package storage

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	mc "github.com/ianoshen/gomc"
	"io"
	"mozilla.org/simplepush/sperrors"
	"mozilla.org/util"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channel record status constants.
const (
	DELETED = iota
	LIVE
	REGISTERED
)

// Common adapter errors.
var (
	ErrPoolSaturated      StorageError = "Connection pool saturated"
	ErrStatusFailed       StorageError = "Invalid value returned"
	ErrUnknownUAID        StorageError = "Unknown UAID for host"
	ErrNoElastiCache      StorageError = "ElastiCache returned no endpoints"
	ErrElastiCacheTimeout StorageError = "ElastiCache query timed out"
)

// cr is a channel record. The short type and field names are used to reduce
// the size of the encoded Gob structure.
type cr struct {
	S int8   //State
	V uint64 // Version
	L int64  // Last touched
}

// ia is a list of decoded channel IDs, so named because the binary Gob encoder
// stores type names.
type ia [][]byte

// Len returns the length of the channel ID slice. Implements
// `sort.Interface.Len()`.
func (channelIDs ia) Len() int {
	return len(channelIDs)
}

// Swap swaps two channel ID slices at the corresponding indices. Implements
// `sort.Interface.Swap()`.
func (channelIDs ia) Swap(i, j int) {
	channelIDs[i], channelIDs[j] = channelIDs[j], channelIDs[i]
}

// Less indicates whether one channel ID slice lexicographically precedes the
// other. Implements `sort.Interface.Less()`.
func (channelIDs ia) Less(i, j int) bool {
	return bytes.Compare(channelIDs[i], channelIDs[j]) < 0
}

// IndexOf returns the location of a channel ID slice in the slice of channel
// IDs, or -1 if the ID isn't present in the containing slice.
func (channelIDs ia) IndexOf(val []byte) int {
	for index, v := range channelIDs {
		if bytes.Equal(v, val) {
			return index
		}
	}
	return -1
}

// Returns a new slice with the string at position pos removed or
// an equivalent slice if the pos is not in the bounds of the slice
func remove(list [][]byte, pos int) (res [][]byte) {
	if pos < 0 || pos == len(list) {
		return list
	}
	return append(list[:pos], list[pos+1:]...)
}

// Use the AWS system to query for the endpoints to use.
// (Allows for dynamic endpoint assignments)
func getElastiCacheEndpoints(configEndpoint string) (string, error) {
	c, err := net.Dial("tcp", configEndpoint)
	if err != nil {
		return "", err
	}
	defer c.Close()

	reader, writer := bufio.NewReader(c), bufio.NewWriter(c)
	writer.Write([]byte("config get cluster\r\n"))
	writer.Flush()

	reader.ReadString('\n')
	reader.ReadString('\n')
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", nil
	}

	endPoints := strings.Split(line, " ")
	if len(endPoints) < 1 {
		return "", ErrNoElastiCache
	}

	var retEndpoints []string
	for _, v := range endPoints {
		endPoint := strings.Split(v, "|")
		if len(endPoint) < 3 {
			continue
		}
		retEndpoints = append(retEndpoints, fmt.Sprintf("%s:%s", endPoint[1], strings.TrimSpace(endPoint[2])))
	}
	return strings.Join(retEndpoints, ","), nil
}

func getElastiCacheEndpointsTimeout(configEndpoint string, seconds int) (string, error) {
	type strErr struct {
		ep  string
		err error
	}

	ch := make(chan strErr, 1)

	go func() {
		ep, err := getElastiCacheEndpoints(configEndpoint)
		ch <- strErr{ep, err}
	}()
	select {
	case se := <-ch:
		return se.ep, se.err
	case <-time.After(time.Duration(seconds) * time.Second):
		return "", ErrElastiCacheTimeout
	}

}

// Determines whether the given error is a memcached "missing key" error.
func isMissing(err error) bool {
	return strings.Contains("NOT FOUND", err.Error())
}

// Converts a `(uaid, chid)` tuple to a binary primary key.
func toBinaryKey(uaid, chid []byte) ([]byte, error) {
	key := make([]byte, 32)
	aoff := 16 - len(uaid)
	if aoff < 0 {
		aoff = 0
	}
	boff := 32 - len(chid)
	if boff < 16 {
		boff = 16
	}
	copy(key[aoff:], uaid)
	copy(key[boff:], chid)
	return key, nil
}

// Converts a binary primary key into a Base64-encoded string suitable for
// storage in memcached.
func encodeKey(key []byte) string {
	// Sadly, can't use full byte chars for key values, so have to encode
	// to base64. Ideally, this would just be
	// return string(key)
	return base64.StdEncoding.EncodeToString(key)
}

// FreeClient wraps a memcached connection with pool information.
type FreeClient struct {
	mc.Client
	releases chan *FreeClient
}

// NewEmcee creates an unconfigured memcached adapter.
func NewEmcee() Adapter {
	s := &EmceeStore{
		closeSignal:  make(chan bool),
		clients:      make(chan mc.Client),
		releases:     make(chan *FreeClient),
		acquisitions: make(chan chan *FreeClient),
	}
	s.closeWait.Add(1)
	go s.run()
	return s
}

// EmceeConfig specifies options for the memcached adapter.
type EmceeConfig struct {
	// Hosts is a list of memcached nodes.
	Hosts []string `toml:"server"`

	// MinConns is the desired number of initial connections. Defaults to 100.
	MinConns int `toml:"pool_size"`

	// MaxConns is the maximum number of open connections managed by the pool.
	// All returned connections that exceed this limit will be closed. Defaults
	// to 400.
	MaxConns int `toml:"max_pool"`

	// Host is the default host name for tracking client registrations. Defaults
	// to `localhost`. TODO: This should be managed by the `Application`, not the
	// storage adapter.
	Host string `toml:"default_host"`

	// HostPrefix is the key prefix for client hosts. Defaults to `"_h-"`.
	HostPrefix string `toml:"shard_prefix"`

	// PingPrefix is the key prefix for proprietary (GCM, etc.) pings. Defaults
	// to `"_pc-"`.
	PingPrefix string `toml:"prop_prefix"`

	// PoolTimeout is the maximum time to wait when acquiring a connection from
	// the pool. Defaults to 5 seconds.
	PoolTimeout time.Duration `toml:"handle_timeout"`

	// ReceiveTimeout is the socket receive timeout (`SO_RCVTIMEO`) used by the
	// memcached driver. Supports microsecond granularity; defaults to 5 seconds.
	ReceiveTimeout time.Duration `toml:"recv_timeout"`

	// SendTimeout is the socket send timeout (`SO_SNDTIMEO`) used by the
	// memcached driver. Supports microsecond granularity; defaults to 5 seconds.
	SendTimeout time.Duration `toml:"send_timeout"`

	// PollTimeout is the `poll()` timeout used by the memcached driver. Supports
	// millisecond granularity; defaults to 5 seconds.
	PollTimeout time.Duration `toml:"poll_timeout"`

	// RetryTimeout is the time to wait before retrying a request on an unhealthy
	// memcached node. Supports second granularity; defaults to 5 seconds.
	RetryTimeout time.Duration `toml:"retry_timeout"`

	// ChannelTimeout is the active channel record timeout. Defaults to 3 days.
	ChannelTimeout time.Duration `toml:"timeout_live"`

	// RegisterTimeout is the registered channel record timeout. Defaults to 3
	// hours; an app server should send a notification on a registered channel
	// before this timeout.
	RegisterTimeout time.Duration `toml:"timeout_reg"`

	// UnregisterTimeout is the deleted channel record timeout. Defaults to
	// 1 day; deleted records will be pruned after this timeout.
	UnregisterTimeout time.Duration `toml:"timeout_del"`
}

// EmceeStore is a memcached adapter.
type EmceeStore struct {
	config       *EmceeConfig
	closeWait    sync.WaitGroup
	closeSignal  chan bool
	closeLock    sync.Mutex
	isClosing    bool
	clients      chan mc.Client
	releases     chan *FreeClient
	acquisitions chan chan *FreeClient
	lastErr      error
}

// ConfigStruct returns a configuration object with defaults. Implements
// `Adapter.ConfigStruct()`. TODO: Should be defined by `HasConfigStruct`.
func (*EmceeStore) ConfigStruct() interface{} {
	return &EmceeConfig{
		MinConns:          100,
		MaxConns:          400,
		Host:              "localhost",
		HostPrefix:        "_h-",
		PingPrefix:        "_pc-",
		PoolTimeout:       5 * time.Second,
		ReceiveTimeout:    5 * time.Second,
		SendTimeout:       5 * time.Second,
		PollTimeout:       5 * time.Second,
		RetryTimeout:      5 * time.Second,
		ChannelTimeout:    3 * 24 * time.Hour,
		RegisterTimeout:   3 * time.Hour,
		UnregisterTimeout: 1 * 24 * time.Hour,
	}
}

// Init initializes the memcached adapter with the given configuration and
// seeds the pool with `MinConns` connections. Implements `Adapter.Init()`.
// TODO: Should be defined by the `NeedsInit` interface.
func (s *EmceeStore) Init(config interface{}) error {
	s.config = config.(*EmceeConfig)
	for index := 0; index < s.config.MinConns; index++ {
		client, err := s.newClient()
		if err != nil {
			s.fatal(err)
			return err
		}
		s.clients <- client
	}
	return nil
}

// Close closes the connection pool and unblocks all pending operations with
// errors. Safe to call multiple times. Implements `Adapter.Close()`.
func (s *EmceeStore) Close() (err error) {
	err, ok := s.stop()
	if !ok {
		return err
	}
	s.closeWait.Wait()
	return
}

// KeyToIDs extracts the hex-encoded device and channel IDs from a user-
// readable primary key. Implements `Adapter.KeyToIDs()`.
func (*EmceeStore) KeyToIDs(key string) (suaid, schid string, ok bool) {
	items := strings.SplitN(key, ".", 2)
	if ok = len(items) == 2; ok {
		suaid, schid = items[0], items[1]
	}
	return
}

// IDsToKey generates a user-readable primary key from a (device ID, channel
// ID) tuple. The primary key is encoded in the push endpoint URI. Implements
// `Adapter.IDsToKey()`.
func (*EmceeStore) IDsToKey(suaid, schid string) (key string, ok bool) {
	if ok = len(suaid) > 0 && len(schid) > 0; ok {
		key = fmt.Sprintf("%s.%s", suaid, schid)
	}
	return
}

// Status queries whether memcached is available for reading and writing.
// Implements `Adapter.Status().
func (s *EmceeStore) Status() (success bool, err error) {
	defer func() {
		if recv := recover(); recv != nil {
			success = false
			err = recv.(error)
			return
		}
	}()
	fakeID, _ := util.GenUUID4()
	key := "status_" + fakeID
	client, err := s.getClient()
	if err != nil {
		return false, err
	}
	defer s.releaseClient(client)
	err = client.Set(key, "test", 6*time.Second)
	if err != nil {
		return false, err
	}
	var val string
	err = client.Get(key, &val)
	if err != nil || val != "test" {
		return false, ErrStatusFailed
	}
	client.Delete(key, 0)
	return true, nil
}

// Exists returns a Boolean indicating whether a device has previously
// registered with the Simple Push server. Implements `Adapter.Exists()`.
func (s *EmceeStore) Exists(uaid []byte) bool {
	_, err := s.fetchAppIDArray(uaid)
	return err == nil
}

// Register creates and stores a channel record for the given device ID and
// channel ID. If the channel `version` is > 0, the record will be marked as
// active. Implements `Adapter.Register()`.
func (s *EmceeStore) Register(uaid, chid []byte, version int64) error {
	if len(uaid) == 0 {
		return sperrors.NoChannelError
	}
	chids, err := s.fetchAppIDArray(uaid)
	if err != nil {
		return err
	}
	if chids.IndexOf(chid) < 0 {
		err = s.storeAppIDArray(uaid, append(chids, chid))
		if err != nil {
			return err
		}
	}
	rec := &cr{
		S: REGISTERED,
		L: time.Now().UTC().Unix()}
	if version != 0 {
		rec.V = uint64(version)
		rec.S = LIVE
	}
	key, err := toBinaryKey(uaid, chid)
	if err != nil {
		return sperrors.InvalidPrimaryKeyError
	}
	err = s.storeRec(key, rec)
	if err != nil {
		return err
	}
	return nil
}

// Update updates the version for the given device ID and channel ID.
// Implements `Adapter.Update()`.
func (s *EmceeStore) Update(uaid, chid []byte, version int64) error {
	if len(uaid) == 0 || len(chid) == 0 {
		return sperrors.InvalidPrimaryKeyError
	}
	key, err := toBinaryKey(uaid, chid)
	if err != nil {
		return sperrors.InvalidPrimaryKeyError
	}
	keyString := hex.EncodeToString(key)
	cRec, err := s.fetchRec(key)
	if err != nil && !isMissing(err) {
		s.Error("fetchRec error", util.Fields{"primarykey": keyString,
			"error": err.Error()})
		return err
	}
	if cRec != nil {
		s.Debug("Replacing record", util.Fields{"primarykey": keyString})
		if cRec.S != DELETED {
			newRecord := &cr{
				V: uint64(version),
				S: LIVE,
				L: time.Now().UTC().Unix()}
			err = s.storeRec(key, newRecord)
			if err != nil {
				return err
			}
			return nil
		}
	}
	// No record found or the record setting was DELETED
	s.Debug("Registering channel", util.Fields{"uaid": hex.EncodeToString(uaid),
		"channelID": hex.EncodeToString(chid),
		"version":   strconv.FormatInt(version, 10)})
	err = s.Register(uaid, chid, version)
	if err != nil {
		return err
	}
	return nil
}

// Unregister marks the channel ID associated with the given device ID
// as inactive. Implements `Adapter.Unregister()`.
func (s *EmceeStore) Unregister(uaid, chid []byte) error {
	chids, err := s.fetchAppIDArray(uaid)
	if err != nil {
		return err
	}
	pos := chids.IndexOf(chid)
	if pos < 0 {
		return sperrors.InvalidChannelError
	}
	key, err := toBinaryKey(uaid, chid)
	if err != nil {
		return err
	}
	if err := s.storeAppIDArray(uaid, remove(chids, pos)); err != nil {
		return err
	}
	// TODO: Allow `MaxRetries` to be configurable.
	for x := 0; x < 3; x++ {
		channel, err := s.fetchRec(key)
		if err != nil {
			s.Error("Could not delete Channel", util.Fields{"primarykey": hex.EncodeToString(key),
				"error": err.Error()})
			continue
		}
		channel.S = DELETED
		err = s.storeRec(key, channel)
		break
	}
	// TODO: Propagate errors.
	return nil
}

// Drop removes a channel ID associated with the given device ID from
// memcached. Deregistration calls should use `Unregister()` instead.
// Implements `Adapter.Drop()`.
func (s *EmceeStore) Drop(uaid, chid []byte) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	key, err := toBinaryKey(uaid, chid)
	if err != nil {
		return err
	}
	err = client.Delete(encodeKey(key), 0)
	if err == nil || isMissing(err) {
		return nil
	}
	return err
}

// FetchAll returns all channel updates and expired channels for a device ID
// since the specified cutoff time. Implements `Adapter.FetchAll()`.
func (s *EmceeStore) FetchAll(uaid []byte, since time.Time) ([]Update, [][]byte, error) {
	chids, err := s.fetchAppIDArray(uaid)
	if err != nil {
		return nil, nil, err
	}

	updates := make([]Update, 0, 20)
	expired := make([][]byte, 0, 20)
	keys := make([]string, 0, 20)

	for _, chid := range chids {
		key, _ := toBinaryKey(uaid, chid)
		keys = append(keys, encodeKey(key))
	}
	deviceString := hex.EncodeToString(uaid)
	s.Debug("Fetching items", util.Fields{"uaid": deviceString,
		"items": "[" + strings.Join(keys, ", ") + "]"})
	client, err := s.getClient()
	if err != nil {
		return nil, nil, err
	}
	defer s.releaseClient(client)

	sinceUnix := since.Unix()
	for index, key := range keys {
		channel := new(cr)
		if err := client.Get(key, channel); err != nil {
			continue
		}
		chid := chids[index]
		channelString := hex.EncodeToString(chid)
		s.Debug("GetUpdates Fetched record ", util.Fields{"uaid": deviceString,
			"chid":  channelString,
			"value": fmt.Sprintf("%d,%d,%d", channel.L, channel.S, channel.V)})
		if channel.L < sinceUnix {
			s.Debug("Skipping record...", util.Fields{"uaid": deviceString, "chid": channelString})
			continue
		}
		// Yay! Go translates numeric interface values as float64s
		// Apparently float64(1) != int(1).
		switch channel.S {
		case LIVE:
			version := channel.V
			if version == 0 {
				version = uint64(time.Now().UTC().Unix())
				s.Error("GetUpdates Using Timestamp", util.Fields{"uaid": deviceString, "chid": channelString})
			}
			update := Update{
				ChannelID: channelString,
				Version:   version,
			}
			updates = append(updates, update)
		case DELETED:
			s.Info("GetUpdates Deleting record", util.Fields{"uaid": deviceString, "chid": channelString})
			expired = append(expired, chid)
		case REGISTERED:
			// Item registered, but not yet active. Ignore it.
		default:
			s.Warn("Unknown state", util.Fields{"uaid": deviceString, "chid": channelString})
		}
	}
	return updates, expired, nil
}

// DropAll removes all channel records for the given device ID. Implements
// `Adapter.DropAll()`.
func (s *EmceeStore) DropAll(uaid []byte) error {
	chids, err := s.fetchAppIDArray(uaid)
	if err != nil {
		return err
	}
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	for _, chid := range chids {
		key, err := toBinaryKey(uaid, chid)
		if err != nil {
			return err
		}
		client.Delete(encodeKey(key), 0)
	}
	if err = client.Delete(encodeKey(uaid), 0); err != nil && !isMissing(err) {
		return err
	}
	if err = client.Delete(s.config.HostPrefix+hex.EncodeToString(uaid), 0); err != nil && !isMissing(err) {
		return err
	}
	return nil
}

// FetchPing retrieves proprietary ping information for the given device ID
// from memcached. Implements `Adapter.FetchPing()`.
func (s *EmceeStore) FetchPing(uaid []byte) (connect string, err error) {
	client, err := s.getClient()
	if err != nil {
		return
	}
	defer s.releaseClient(client)
	err = client.Get(s.config.PingPrefix+hex.EncodeToString(uaid), &connect)
	return
}

// PutPing stores the proprietary ping info blob for the given device ID in
// memcached. Implements `Adapter.PutPing()`.
func (s *EmceeStore) PutPing(uaid []byte, connect string) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	return client.Set(s.config.PingPrefix+hex.EncodeToString(uaid), connect, 0)
}

// DropPing removes all proprietary ping info for the given device ID.
// Implements `Adapter.DropPing()`.
func (s *EmceeStore) DropPing(uaid []byte) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	return client.Delete(s.config.PingPrefix+hex.EncodeToString(uaid), 0)
}

// FetchHost returns the host name of the Simple Push server that currently
// maintains a connection to the device. Implements `Adapter.FetchHost()`.
func (s *EmceeStore) FetchHost(uaid []byte) (host string, err error) {
	deviceString := hex.EncodeToString(uaid)
	defer func(defaultHost string) {
		if err := recover(); err != nil {
			s.Error("GetUAIDHost no host", util.Fields{"uaid": deviceString,
				"error": err.(error).Error()})
		}
	}(s.config.Host)
	client, err := s.getClient()
	if err != nil {
		return s.config.Host, err
	}
	defer s.releaseClient(client)
	err = client.Get(s.config.HostPrefix+deviceString, &host)
	if err != nil {
		if isMissing(err) {
			return s.config.Host, ErrUnknownUAID
		}
		s.Error("GetUAIDHost Fetch error", util.Fields{"uaid": deviceString,
			"item":  host,
			"error": err.Error()})
		return s.config.Host, err
	}
	if len(host) == 0 {
		host = s.config.Host
	}
	s.Debug("GetUAIDHost", util.Fields{"uaid": deviceString, "host": host})
	s.storeHost(client, deviceString, host)
	return
}

// PutHost updates the host name associated with the device ID. Implements
// `Adapter.PutHost()`.
func (s *EmceeStore) PutHost(uaid []byte, host string) error {
	if len(host) == 0 {
		host = s.config.Host
	}
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	return s.storeHost(client, hex.EncodeToString(uaid), host)
}

// DropHost removes the host mapping for the given device ID from memcached.
// Implements `Adapter.DropHost()`.
func (s *EmceeStore) DropHost(uaid []byte) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	err = client.Delete(s.config.HostPrefix+hex.EncodeToString(uaid), 0)
	if err == nil || isMissing(err) {
		return nil
	}
	return err
}

// Error logs an error.
func (s *EmceeStore) Error(message string, fields util.Fields) {}

// Debug logs a debug message.
func (s *EmceeStore) Debug(message string, fields util.Fields) {}

// Info logs an info message.
func (s *EmceeStore) Info(message string, fields util.Fields) {}

// Warn logs a warning message.
func (s *EmceeStore) Warn(message string, fields util.Fields) {}

// Queries memcached for a list of current subscriptions associated with the
// given device ID.
func (s *EmceeStore) fetchAppIDArray(uaid []byte) (result ia, err error) {
	if len(uaid) == 0 {
		return nil, nil
	}
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	defer s.releaseClient(client)
	err = client.Get(encodeKey(uaid), &result)
	if err != nil {
		// TODO: Returning successful responses for missing keys causes `Exists()` to
		// return `true` for all device IDs. Verify if correcting this behavior
		// breaks existing clients.
		if isMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	// pare out duplicates.
	for i, chid := range result {
		if dup := result[i+1:].IndexOf(chid); dup > -1 {
			result = remove(result, i+dup)
		}
	}
	return
}

// Writes an updated subscription list for the given device ID to memcached.
// `arr` is sorted in-place.
func (s *EmceeStore) storeAppIDArray(uaid []byte, arr ia) error {
	if len(uaid) == 0 {
		return sperrors.MissingDataError
	}
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	// sort the array
	sort.Sort(arr)
	err = client.Set(encodeKey(uaid), arr, 0)
	if err != nil {
		return err
	}
	return nil
}

// Retrieves a channel record from memcached.
func (s *EmceeStore) fetchRec(pk []byte) (*cr, error) {
	if len(pk) == 0 {
		return nil, sperrors.InvalidPrimaryKeyError
	}
	keyString := encodeKey(pk)
	defer func() {
		if err := recover(); err != nil {
			s.Error("could not fetch record", util.Fields{"primarykey": keyString,
				"error": err.(error).Error()})
		}
	}()
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	defer s.releaseClient(client)
	result := new(cr)
	err = client.Get(keyString, result)
	if err != nil && !isMissing(err) {
		s.Error("Get Failed", util.Fields{"primarykey": keyString,
			"error": err.Error()})
		return nil, err
	}
	s.Debug("Fetched", util.Fields{"primarykey": keyString,
		"result": fmt.Sprintf("state: %d, vers: %d, last: %d",
			result.S, result.V, result.L),
	})
	return result, nil
}

// Stores an updated channel record in memcached.
func (s *EmceeStore) storeRec(pk []byte, rec *cr) error {
	if len(pk) == 0 {
		return sperrors.InvalidPrimaryKeyError
	}
	if rec == nil {
		return sperrors.NoDataToStoreError
	}
	var ttl time.Duration
	switch rec.S {
	case DELETED:
		ttl = s.config.UnregisterTimeout
	case REGISTERED:
		ttl = s.config.RegisterTimeout
	default:
		ttl = s.config.ChannelTimeout
	}
	rec.L = time.Now().UTC().Unix()
	client, err := s.getClient()
	if err != nil {
		return err
	}
	defer s.releaseClient(client)
	keyString := encodeKey(pk)
	err = client.Set(keyString, rec, ttl)
	if err != nil {
		s.Error("Failure to set item", util.Fields{"primarykey": keyString,
			"error": err.Error()})
	}
	return nil
}

// Releases an acquired memcached connection. TODO: The run loop should
// ensure that the `FreeClient` is in a valid state. If a connection error
// occurs, the client should be closed and a new connection opened as needed.
// Otherwise, `getClient()` may return a bad client connection.
func (s *EmceeStore) releaseClient(client *FreeClient) {
	if client == nil {
		return
	}
	client.releases <- client
}

// Acquires a memcached connection from the connection pool.
func (s *EmceeStore) getClient() (*FreeClient, error) {
	freeClients := make(chan *FreeClient)
	select {
	case <-s.closeSignal:
		return nil, io.EOF
	case s.acquisitions <- freeClients:
		if client := <-freeClients; client != nil {
			return client, nil
		}
	case <-time.After(s.config.PoolTimeout):
	}
	return nil, ErrPoolSaturated
}

// Creates and configures a memcached client connection.
func (s *EmceeStore) newClient() (mc.Client, error) {
	client, err := mc.NewClient(s.config.Hosts, 1, mc.ENCODING_GOB)
	if err != nil {
		return nil, err
	}
	// internally hash key using MD5 (for key distribution)
	if err := client.SetBehavior(mc.BEHAVIOR_KETAMA_HASH, 1); err != nil {
		client.Close()
		return nil, err
	}
	// Use the binary protocol, which allows us faster data xfer
	// and better data storage (can use full UTF-8 char space)
	if err := client.SetBehavior(mc.BEHAVIOR_BINARY_PROTOCOL, 1); err != nil {
		client.Close()
		return nil, err
	}
	// The send and receive timeouts are expressed in microseconds.
	// `SetBehavior()` wraps libmemcached's `memcached_behavior_set()` call.
	sendTimeout := uint64(s.config.SendTimeout / time.Microsecond)
	if err := client.SetBehavior(mc.BEHAVIOR_SND_TIMEOUT, sendTimeout); err != nil {
		client.Close()
		return nil, err
	}
	receiveTimeout := uint64(s.config.ReceiveTimeout / time.Microsecond)
	if err := client.SetBehavior(mc.BEHAVIOR_RCV_TIMEOUT, receiveTimeout); err != nil {
		client.Close()
		return nil, err
	}
	// `poll(2)` accepts a millisecond timeout.
	pollTimeout := uint64(s.config.PollTimeout / time.Millisecond)
	if err := client.SetBehavior(mc.BEHAVIOR_POLL_TIMEOUT, pollTimeout); err != nil {
		client.Close()
		return nil, err
	}
	// The memcached retry timeout is expressed in seconds.
	retryTimeout := uint64(s.config.RetryTimeout / time.Second)
	if err = client.SetBehavior(mc.BEHAVIOR_RETRY_TIMEOUT, retryTimeout); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// Stores host information in memcached.
func (s *EmceeStore) storeHost(client mc.Client, uaid, host string) error {
	s.Debug("storeHost", util.Fields{"uaid": uaid, "host": host})
	err := client.Set(s.config.HostPrefix+uaid, host, s.config.ChannelTimeout)
	if err == nil || isMissing(err) {
		return nil
	}
	return err
}

// The store run loop.
func (s *EmceeStore) run() {
	defer s.closeWait.Done()
	clients := list.New()
	capacity := 0
	for ok := true; ok; {
		select {
		case ok = <-s.closeSignal:
		case client := <-s.clients:
			if capacity >= s.config.MaxConns {
				client.Close()
				break
			}
			clients.PushBack(&FreeClient{client, s.releases})
			capacity++

		case freeClient := <-s.releases:
			if capacity >= s.config.MaxConns {
				// Maximum pool size exceeded; close the connection.
				freeClient.Close()
				break
			}
			clients.PushBack(freeClient)

		case acquisition := <-s.acquisitions:
			if clients.Len() > 0 {
				// Return the first available connection from the pool.
				if client, ok := clients.Remove(clients.Front()).(*FreeClient); ok {
					acquisition <- client
				}
				close(acquisition)
				break
			}
			if capacity < s.config.MaxConns {
				// All connections are in use, but the pool has not reached its maximum
				// capacity. The caller should call `s.releaseClient()` to return the
				// connection to the pool. TODO: Spawning a separate Goroutine to handle
				// connections would avoid blocking the run loop.
				client, err := s.newClient()
				if err != nil {
					s.fatal(err)
					close(acquisition)
					break
				}
				acquisition <- &FreeClient{client, s.releases}
				capacity++
				close(acquisition)
				break
			}
			// Pool saturated.
			close(acquisition)
		}
	}
	// Shut down all connections in the pool.
	for element := clients.Front(); element != nil; element = element.Next() {
		if client, ok := element.Value.(*FreeClient); ok {
			client.Close()
		}
	}
}

// Acquires `s.closeLock`, closes the pool, and releases the lock, reporting
// any errors to the caller. `ok` indicates whether the caller should wait
// for the pool to close before returning.
func (s *EmceeStore) stop() (err error, ok bool) {
	defer s.closeLock.Unlock()
	s.closeLock.Lock()
	if s.isClosing {
		return s.lastErr, false
	}
	return s.signalClose(), true
}

// Acquires `s.closeLock`, closes the connection pool, and releases the lock,
// storing the given error in the `lastErr` field.
func (s *EmceeStore) fatal(err error) {
	defer s.closeLock.Unlock()
	s.closeLock.Lock()
	s.signalClose()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

// Closes the pool and exits the run loop. Assumes the caller holds
// `s.closeLock`.
func (s *EmceeStore) signalClose() (err error) {
	if s.isClosing {
		return
	}
	close(s.closeSignal)
	s.isClosing = true
	return nil
}
