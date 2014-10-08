/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package simplepush

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mozilla-services/heka/client"
	"github.com/mozilla-services/heka/message"
	"github.com/mozilla-services/pushgo/id"
)

// A LogLevel represents a message log level.
type LogLevel int32

// syslog severity levels.
const (
	EMERGENCY LogLevel = iota
	ALERT
	CRITICAL
	ERROR
	WARNING
	NOTICE
	INFO
	DEBUG
)

var levelNames = map[LogLevel]string{
	EMERGENCY: "EMERGENCY",
	ALERT:     "ALERT",
	CRITICAL:  "CRITICAL",
	ERROR:     "ERROR",
	WARNING:   "WARNING",
	NOTICE:    "NOTICE",
	INFO:      "INFO",
	DEBUG:     "DEBUG",
}

func (l LogLevel) String() string {
	return levelNames[l]
}

const (
	HeaderID      = "X-Request-Id"
	TextLogTime   = "2006-01-02 15:04:05 -0700"
	CommonLogTime = "02/Jan/2006:15:04:05 -0700"
)

type LogFields map[string]string

func (l LogFields) Names() (names []string) {
	names = make([]string, len(l))
	i := 0
	for name := range l {
		names[i] = name
		i++
	}
	sort.Strings(names)
	return
}

type Logger interface {
	HasConfigStruct
	Log(level LogLevel, messageType, payload string, fields LogFields) error
	SetFilter(level LogLevel)
	ShouldLog(level LogLevel) bool
	Close() error
}

var AvailableLoggers = make(AvailableExtensions)

type SimpleLogger struct {
	Logger
}

// Error string helper that ignores nil errors
func ErrStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Attempt to convert an interface to a string, ignore failures
func IStr(i interface{}) (reply string) {
	if i == nil {
		return ""
	}
	if reply, ok := i.(string); ok {
		return reply
	}
	return "Undefined"
}

// SimplePush Logger implementation, utilizes the passed in Logger
func NewLogger(log Logger) (*SimpleLogger, error) {
	return &SimpleLogger{log}, nil
}

// Default logging calls for convenience
func (sl *SimpleLogger) Debug(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(DEBUG, mtype, msg, fields)
}

func (sl *SimpleLogger) Info(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(INFO, mtype, msg, fields)
}

func (sl *SimpleLogger) Notice(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(NOTICE, mtype, msg, fields)
}

func (sl *SimpleLogger) Warn(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(WARNING, mtype, msg, fields)
}

func (sl *SimpleLogger) Error(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(ERROR, mtype, msg, fields)
}

func (sl *SimpleLogger) Critical(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(CRITICAL, mtype, msg, fields)
}

func (sl *SimpleLogger) Alert(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(ALERT, mtype, msg, fields)
}

func (sl *SimpleLogger) Panic(mtype, msg string, fields LogFields) error {
	return sl.Logger.Log(EMERGENCY, mtype, msg, fields)
}

// A NetworkLogger sends Protobuf-encoded log messages to a remote Heka
// instance over TCP or UDP.
type NetworkLogger struct {
	Logger
}

type NetworkLoggerConfig struct {
	Proto      string
	Addr       string
	UseTLS     bool   `toml:"use_tls" env:"use_tls"`
	EnvVersion string `toml:"env_version" env:"env_version"`
	Filter     int32
}

func (nl *NetworkLogger) ConfigStruct() interface{} {
	return &NetworkLoggerConfig{
		Proto:      "tcp",
		EnvVersion: "2",
		Filter:     0,
	}
}

func (nl *NetworkLogger) Init(app *Application, config interface{}) (err error) {
	conf := config.(*NetworkLoggerConfig)
	var sender client.Sender
	if conf.UseTLS {
		sender, err = client.NewTlsSender(conf.Proto, conf.Addr, nil)
	} else {
		sender, err = client.NewNetworkSender(conf.Proto, conf.Addr)
	}
	if err != nil {
		return err
	}
	nl.Logger = NewHekaLogger(sender, client.NewProtobufEncoder(nil))
	hekaConf := nl.Logger.ConfigStruct().(*HekaLoggerConfig)
	hekaConf.EnvVersion = conf.EnvVersion
	hekaConf.Filter = conf.Filter
	if err = nl.Logger.Init(app, hekaConf); err != nil {
		return err
	}
	return nil
}

// A FileLogger writes Protobuf-encoded Heka log messages to a local log file.
type FileLogger struct {
	Logger
}

type FileLoggerConfig struct {
	Path       string
	EnvVersion string `toml:"env_version" env:"env_version"`
	Filter     int32
}

func (fl *FileLogger) ConfigStruct() interface{} {
	return &FileLoggerConfig{
		EnvVersion: "2",
		Filter:     0,
	}
}

func (fl *FileLogger) Init(app *Application, config interface{}) (err error) {
	conf := config.(*FileLoggerConfig)
	if len(conf.Path) == 0 {
		return fmt.Errorf("FileLogger: Missing log file path")
	}
	logFile, err := os.OpenFile(conf.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return err
	}
	fl.Logger = NewHekaLogger(&HekaSender{logFile}, client.NewProtobufEncoder(nil))
	hekaConf := fl.Logger.ConfigStruct().(*HekaLoggerConfig)
	hekaConf.EnvVersion = conf.EnvVersion
	hekaConf.Filter = conf.Filter
	if err = fl.Logger.Init(app, hekaConf); err != nil {
		return err
	}
	return nil
}

// NewProtobufLogger creates a logger that writes Protobuf-encoded Heka
// log messages to standard output. Protobuf encoding should be preferred
// over JSON for efficiency.
func NewProtobufLogger() *HekaLogger {
	return NewHekaLogger(&HekaSender{writerOnly{os.Stdout}}, client.NewProtobufEncoder(nil))
}

// NewJSONLogger creates a logger that writes JSON-encoded log messages to
// standard output.
func NewJSONLogger() *HekaLogger {
	return NewHekaLogger(&HekaSender{writerOnly{os.Stdout}}, hekaJSONEncoder)
}

// NewHekaLogger creates a Heka message logger with the specified message
// sender and encoder.
func NewHekaLogger(sender client.Sender, encoder client.StreamEncoder) *HekaLogger {
	return &HekaLogger{
		sender:  sender,
		encoder: encoder,
	}
}

var hekaMessagePool = sync.Pool{New: func() interface{} {
	return &hekaMessage{msg: new(message.Message)}
}}

func newHekaMessage() *hekaMessage {
	return hekaMessagePool.Get().(*hekaMessage)
}

type hekaMessage struct {
	msg      *message.Message
	outBytes []byte
}

func (hm *hekaMessage) free() {
	if cap(hm.outBytes) > 1024 {
		return
	}
	hm.outBytes = hm.outBytes[:0]
	hm.msg.Fields = nil
	hekaMessagePool.Put(hm)
}

type HekaLoggerConfig struct {
	EnvVersion string `toml:"env_version" env:"env_version"`
	Filter     int32
}

type HekaLogger struct {
	sender     client.Sender
	encoder    client.StreamEncoder
	logName    string
	pid        int32
	envVersion string
	hostname   string
	filter     LogLevel
}

func (hl *HekaLogger) ConfigStruct() interface{} {
	return &HekaLoggerConfig{
		EnvVersion: "2",
		Filter:     0,
	}
}

func (hl *HekaLogger) Init(app *Application, config interface{}) (err error) {
	conf := config.(*HekaLoggerConfig)
	hl.pid = int32(os.Getpid())
	hl.hostname = app.Hostname()
	hl.logName = fmt.Sprintf("pushgo-%s", VERSION)
	hl.envVersion = conf.EnvVersion
	hl.filter = LogLevel(conf.Filter)
	return
}

func (hl *HekaLogger) ShouldLog(level LogLevel) bool {
	return level <= hl.filter
}

func (hl *HekaLogger) SetFilter(level LogLevel) {
	hl.filter = level
}

func (hl *HekaLogger) Log(level LogLevel, messageType, payload string, fields LogFields) (err error) {
	if !hl.ShouldLog(level) {
		return
	}
	hm := newHekaMessage()
	defer hm.free()
	messageID, _ := id.GenerateBytes()
	hm.msg.SetUuid(messageID)
	hm.msg.SetTimestamp(time.Now().UnixNano())
	hm.msg.SetType(messageType)
	hm.msg.SetLogger(hl.logName)
	hm.msg.SetSeverity(int32(level))
	hm.msg.SetPayload(payload)
	hm.msg.SetEnvVersion(hl.envVersion)
	hm.msg.SetPid(hl.pid)
	hm.msg.SetHostname(hl.hostname)
	for _, name := range fields.Names() {
		message.NewStringField(hm.msg, name, fields[name])
	}
	if err = hl.encoder.EncodeMessageStream(hm.msg, &hm.outBytes); err != nil {
		log.Fatalf("Error encoding log message: %s", err)
	}
	if err = hl.sender.SendMessage(hm.outBytes); err != nil {
		log.Fatalf("Error sending log message: %s", err)
	}
	return
}

func (hl *HekaLogger) Close() error {
	hl.sender.Close()
	return nil
}

// A HekaSender writes serialized Heka messages to an underlying writer.
type HekaSender struct {
	io.Writer
}

// SendMessage writes a serialized Heka message. Implements
// client.Sender.SendMessage.
func (s *HekaSender) SendMessage(data []byte) (err error) {
	_, err = s.Writer.Write(data)
	return
}

// Close closes the underlying writer. Implements client.Sender.Close.
func (s *HekaSender) Close() {
	if c, ok := s.Writer.(io.Closer); ok {
		c.Close()
	}
}

// A HekaJSONEncoder encodes Heka messages to JSON.
type HekaJSONEncoder struct{}

func (j *HekaJSONEncoder) EncodeMessage(m *message.Message) ([]byte, error) {
	return json.Marshal(m)
}

func (j *HekaJSONEncoder) EncodeMessageStream(m *message.Message, dest *[]byte) (err error) {
	if *dest, err = j.EncodeMessage(m); err != nil {
		return
	}
	*dest = append(*dest, '\n')
	return
}

var hekaJSONEncoder = &HekaJSONEncoder{}

// NewTextLogger creates a logger that writes human-readable log messages to
// standard error.
func NewTextLogger() *TextLogger {
	return &TextLogger{writer: writerOnly{os.Stderr}}
}

type TextLoggerConfig struct {
	Filter int32
	Trace  bool
}

// A TextLogger writes human-readable log messages.
type TextLogger struct {
	pid      int32
	hostname string
	filter   LogLevel
	writer   io.Writer
}

func (tl *TextLogger) ConfigStruct() interface{} {
	return &TextLoggerConfig{
		Filter: 0,
		Trace:  false,
	}
}

func (tl *TextLogger) Init(app *Application, config interface{}) (err error) {
	conf := config.(*TextLoggerConfig)
	tl.pid = int32(os.Getpid())
	tl.hostname = app.Hostname()
	tl.filter = LogLevel(conf.Filter)
	return
}

func (tl *TextLogger) ShouldLog(level LogLevel) bool {
	return level <= tl.filter
}

func (tl *TextLogger) SetFilter(level LogLevel) {
	tl.filter = level
}

func (tl *TextLogger) Log(level LogLevel, messageType, payload string, fields LogFields) (err error) {
	// Return ASAP if we shouldn't be logging
	if !tl.ShouldLog(level) {
		return
	}

	reply := new(bytes.Buffer)
	fmt.Fprintf(reply, "%s [% 8s] %s %s", time.Now().Format(TextLogTime),
		level, messageType, strconv.Quote(payload))

	if len(fields) > 0 {
		reply.WriteByte(' ')
		names := fields.Names()
		for i, name := range names {
			fmt.Fprintf(reply, "%s:%s", name, fields[name])
			if i < len(names)-1 {
				reply.WriteString(", ")
			}
		}
	}

	reply.WriteByte('\n')
	_, err = reply.WriteTo(tl.writer)

	return
}

func (tl *TextLogger) Close() (err error) {
	if c, ok := tl.writer.(io.Closer); ok {
		err = c.Close()
	}
	return
}

// logResponseWriter wraps an http.ResponseWriter, recording its status code,
// content length, and response time.
type logResponseWriter struct {
	http.ResponseWriter
	wroteHeader   bool
	StatusCode    int
	ContentLength int
	RespondedAt   time.Time
}

// writerOnly hides the optional ReadFrom and Close methods of an io.Writer.
// Taken from package net/http, copyright 2009, The Go Authors.
type writerOnly struct {
	io.Writer
}

// ReadFrom calls the ReadFrom method of the underlying ResponseWriter. Defined
// for compatibility with *response.ReadFrom from package net/http.
func (w *logResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if dest, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return dest.ReadFrom(r)
	}
	return io.Copy(writerOnly{w}, r)
}

// WriteHeader records the response time and status code, then calls the
// WriteHeader method of the underlying ResponseWriter.
func (w *logResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.setStatus(statusCode)
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

// setStatus records the response time and status code.
func (w *logResponseWriter) setStatus(statusCode int) {
	w.RespondedAt = time.Now()
	w.StatusCode = statusCode
}

// Write calls the Write method of the underlying ResponseWriter and records
// the number of bytes written.
func (w *logResponseWriter) Write(data []byte) (n int, err error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err = w.ResponseWriter.Write(data)
	w.ContentLength += len(data)
	return
}

// Hijack calls the Hijack method of the underlying ResponseWriter, allowing a
// custom protocol handler (e.g., WebSockets) to take over the connection.
func (w *logResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, io.EOF
	}
	if !w.wroteHeader {
		w.wroteHeader = true
		w.setStatus(http.StatusSwitchingProtocols)
	}
	return hj.Hijack()
}

// CloseNotify calls the CloseNotify method of the underlying ResponseWriter,
// or returns a nil channel if the operation is not supported.
func (w *logResponseWriter) CloseNotify() <-chan bool {
	if cn, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	return nil
}

// Flush calls the Flush method of the underlying ResponseWriter, recording a
// successful response if WriteHeader was not called.
func (w *logResponseWriter) Flush() {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.setStatus(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LogHandler logs the result of an HTTP request.
type LogHandler struct {
	http.Handler
	Log *SimpleLogger
}

// formatRequest generates a Common Log Format request line.
func (h *LogHandler) formatRequest(writer *logResponseWriter, req *http.Request, remoteAddrs []string) string {
	remoteAddr := "-"
	if len(remoteAddrs) > 0 {
		remoteAddr = remoteAddrs[0]
	}
	requestInfo := fmt.Sprintf("%s %s %s", req.Method, req.URL.Path, req.Proto)
	return fmt.Sprintf(`%s - - [%s] %s %d %d`, remoteAddr, writer.RespondedAt.Format(CommonLogTime),
		strconv.Quote(requestInfo), writer.StatusCode, writer.ContentLength)
}

// logResponse logs a response to an HTTP request.
func (h *LogHandler) logResponse(writer *logResponseWriter, req *http.Request, requestID string, receivedAt time.Time) {
	if !h.Log.ShouldLog(INFO) {
		return
	}
	forwardedFor := req.Header[http.CanonicalHeaderKey("X-Forwarded-For")]
	remoteAddrs := make([]string, len(forwardedFor)+1)
	copy(remoteAddrs, forwardedFor)
	remoteAddrs[len(remoteAddrs)-1], _, _ = net.SplitHostPort(req.RemoteAddr)
	h.Log.Info("http", h.formatRequest(writer, req, remoteAddrs), LogFields{
		"rid":                requestID,
		"agent":              req.Header.Get("User-Agent"),
		"path":               req.URL.Path,
		"method":             req.Method,
		"code":               strconv.Itoa(writer.StatusCode),
		"remoteAddressChain": fmt.Sprintf("[%s]", strings.Join(remoteAddrs, ", ")),
		"t":                  strconv.FormatInt(int64(writer.RespondedAt.Sub(receivedAt)/time.Millisecond), 10)})
}

// ServeHTTP implements http.Handler.ServeHTTP.
func (h *LogHandler) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	receivedAt := time.Now()

	// The `X-Request-Id` header is used by Heroku, restify, etc. to correlate
	// logs for the same request.
	requestID := req.Header.Get(HeaderID)
	if !id.Valid(requestID) {
		requestID, _ = id.Generate()
		req.Header.Set(HeaderID, requestID)
	}

	writer := &logResponseWriter{ResponseWriter: res, StatusCode: http.StatusOK}
	defer h.logResponse(writer, req, requestID, receivedAt)

	h.Handler.ServeHTTP(writer, req)
}

func init() {
	AvailableLoggers["protobuf"] = func() HasConfigStruct { return NewProtobufLogger() }
	AvailableLoggers["json"] = func() HasConfigStruct { return NewJSONLogger() }
	AvailableLoggers["net"] = func() HasConfigStruct { return new(NetworkLogger) }
	AvailableLoggers["file"] = func() HasConfigStruct { return new(FileLogger) }
	AvailableLoggers["text"] = func() HasConfigStruct { return NewTextLogger() }
	AvailableLoggers["default"] = AvailableLoggers["text"]
}
