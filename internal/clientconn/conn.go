// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package clientconn provides wire protocol server implementation.
package clientconn

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/pmezard/go-difflib/difflib"
	"go.opentelemetry.io/otel"
	otelattribute "go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/FerretDB/FerretDB/v2/internal/clientconn/conninfo"
	"github.com/FerretDB/FerretDB/v2/internal/clientconn/connmetrics"
	"github.com/FerretDB/FerretDB/v2/internal/handler/middleware"
	"github.com/FerretDB/FerretDB/v2/internal/handler/proxy"
	"github.com/FerretDB/FerretDB/v2/internal/mongoerrors"
	"github.com/FerretDB/FerretDB/v2/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/v2/internal/util/logging"
	"github.com/FerretDB/FerretDB/v2/internal/util/must"
	"github.com/FerretDB/FerretDB/v2/internal/util/observability"
)

// conn represents client connection.
type conn struct {
	netConn        net.Conn
	mode           middleware.Mode
	l              *slog.Logger
	h              middleware.Handler
	m              *connmetrics.ConnMetrics
	proxy          *proxy.Handler
	lastRequestID  atomic.Int32
	testRecordsDir string // if empty, no records are created
}

// newConnOpts represents newConn options.
type newConnOpts struct {
	netConn     net.Conn
	mode        middleware.Mode
	l           *slog.Logger
	handler     middleware.Handler
	connMetrics *connmetrics.ConnMetrics

	proxyAddr        string
	proxyTLSCertFile string
	proxyTLSKeyFile  string
	proxyTLSCAFile   string

	testRecordsDir string // if empty, no records are created
}

// newConn creates a new client connection for given net.Conn.
func newConn(opts *newConnOpts) (*conn, error) {
	if opts.mode == "" {
		panic("mode required")
	}
	if opts.handler == nil {
		panic("handler required")
	}

	var p *proxy.Handler

	if opts.mode != middleware.NormalMode {
		var err error

		proxyOpts := &proxy.NewOpts{
			Addr:     opts.proxyAddr,
			CertFile: opts.proxyTLSCertFile,
			KeyFile:  opts.proxyTLSKeyFile,
			CAFile:   opts.proxyTLSCAFile,
			L:        logging.WithName(opts.l, "proxy"),
		}
		p, err = proxy.New(proxyOpts)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	return &conn{
		netConn:        opts.netConn,
		mode:           opts.mode,
		l:              opts.l,
		h:              opts.handler,
		m:              opts.connMetrics,
		proxy:          p,
		testRecordsDir: opts.testRecordsDir,
	}, nil
}

// run runs the client connection until ctx is canceled, client disconnects,
// or fatal error or panic is encountered.
//
// Returned error is always non-nil.
//
// The caller is responsible for closing the underlying net.Conn.
func (c *conn) run(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer func() {
		cancel(lazyerrors.Errorf("run exits: %w", err))
	}()

	connInfo := conninfo.New()

	defer connInfo.Close()

	if c.netConn.RemoteAddr().Network() != "unix" {
		connInfo.Peer, err = netip.ParseAddrPort(c.netConn.RemoteAddr().String())
		if err != nil {
			return
		}
	}

	ctx = conninfo.Ctx(ctx, connInfo)

	// That's not the best – it makes proxy handler very different from the main handler.
	// Instead, proxy handler should map connections based on connInfo.
	// TODO https://github.com/FerretDB/FerretDB/issues/4965
	if c.proxy != nil {
		go c.proxy.Run(ctx)
	}

	done := make(chan struct{})

	// handle ctx cancellation
	go func() {
		select {
		case <-done:
			// nothing, let goroutine exit
		case <-ctx.Done():
			// unblocks ReadMessage below; any non-zero past value will do
			if e := c.netConn.SetDeadline(time.Unix(0, 0)); e != nil {
				c.l.WarnContext(ctx, fmt.Sprintf("Failed to set deadline: %s", e))
			}
		}
	}()

	defer func() {
		if p := recover(); p != nil {
			c.l.LogAttrs(ctx, logging.LevelDPanic, fmt.Sprint(p), logging.Error(err))
			err = errors.New("panic")
		}

		// let goroutine above exit
		close(done)
	}()

	bufr := bufio.NewReader(c.netConn)

	// if test record path is set, split netConn reader to write to file and bufr
	if c.testRecordsDir != "" {
		if err = os.MkdirAll(c.testRecordsDir, 0o777); err != nil {
			return
		}

		// write to temporary file first, then rename to avoid partial files

		// use local directory so os.Rename below always works
		var f *os.File
		if f, err = os.CreateTemp(c.testRecordsDir, "_*.partial"); err != nil {
			return
		}

		h := sha256.New()

		defer func() {
			c.renamePartialFile(ctx, f, h, err)
		}()

		r := io.TeeReader(c.netConn, io.MultiWriter(f, h))
		bufr = bufio.NewReader(r)
	}

	bufw := bufio.NewWriter(c.netConn)

	defer func() {
		if e := bufw.Flush(); err == nil {
			err = e
		}

		// c.netConn is closed by the caller
	}()

	for {
		if err = c.processMessage(ctx, bufr, bufw); err != nil {
			return
		}
	}
}

// processMessage reads the request, routes the request based on the operation mode
// and writes the response.
//
// Any error returned indicates the connection should be closed.
func (c *conn) processMessage(ctx context.Context, bufr *bufio.Reader, bufw *bufio.Writer) error {
	reqHeader, reqBody, err := wire.ReadMessage(bufr)
	if err != nil {
		return err
	}

	if c.l.Enabled(ctx, slog.LevelDebug) {
		c.l.DebugContext(ctx, "Request header: "+reqHeader.String())
		c.l.DebugContext(ctx, "Request message:\n"+reqBody.StringIndent())
	}

	// diffLogLevel provides the level of logging for the diff between the "normal" and "proxy" responses.
	// It is set to the highest level of logging used to log response.
	diffLogLevel := slog.LevelDebug

	// send request to proxy first (unless we are in normal mode)
	// because FerretDB's handling could modify reqBody's documents,
	// creating a data race
	var proxyHeader *wire.MsgHeader
	var proxyBody wire.MsgBody

	if c.mode != middleware.NormalMode {
		if c.proxy == nil {
			panic("proxy addr was nil")
		}

		// TODO https://github.com/FerretDB/FerretDB/issues/1997
		var req *middleware.Request

		if req, err = middleware.RequestWire(reqHeader, reqBody); err != nil {
			return lazyerrors.Error(err)
		}

		var resp *middleware.Response

		if resp, err = c.proxy.Handle(ctx, req); err != nil {
			return lazyerrors.Error(err)
		}

		proxyHeader = resp.WireHeader()
		proxyBody = resp.WireBody()
	}

	// handle request unless we are in proxy mode
	var resCloseConn bool
	var resHeader *wire.MsgHeader
	var resBody wire.MsgBody

	if c.mode != middleware.ProxyMode {
		resHeader, resBody, resCloseConn = c.route(ctx, reqHeader, reqBody)
		if level := c.logResponse(ctx, "Response", resHeader, resBody, resCloseConn); level > diffLogLevel {
			diffLogLevel = level
		}
	}

	// log proxy response after the normal response to make it less confusing
	if c.mode != middleware.NormalMode {
		if level := c.logResponse(ctx, "Proxy response", proxyHeader, proxyBody, false); level > diffLogLevel {
			diffLogLevel = level
		}
	}

	// diff in diff mode
	if c.l.Enabled(ctx, diffLogLevel) && (c.mode == middleware.DiffNormalMode || c.mode == middleware.DiffProxyMode) {
		if err = c.logDiff(ctx, resHeader, proxyHeader, resBody, proxyBody, diffLogLevel); err != nil {
			return err
		}
	}

	// replace response with one from proxy in proxy and diff-proxy modes
	if c.mode == middleware.ProxyMode || c.mode == middleware.DiffProxyMode {
		resHeader = proxyHeader
		resBody = proxyBody
	}

	if resHeader == nil || resBody == nil {
		panic("no response to send to client")
	}

	if err = wire.WriteMessage(bufw, resHeader, resBody); err != nil {
		c.l.DebugContext(ctx, "Failed to write message", logging.Error(err))

		return err
	}

	if err = bufw.Flush(); err != nil {
		c.l.DebugContext(ctx, "Failed to flush buffer", logging.Error(err))

		return err
	}

	if resCloseConn {
		err = errors.New("fatal error")

		c.l.DebugContext(ctx, "Connection closed unexpectedly", logging.Error(err))

		return err
	}

	return nil
}

// route sends request to a handler's command based on the op code provided in the request header.
//
// The passed context is canceled when the client disconnects.
//
// Handlers to which it routes, should not panic on bad input, but may do so in "impossible" cases.
// They also should not use recover(). That allows us to use fuzzing.
//
// Returned resBody can be nil.
func (c *conn) route(connCtx context.Context, reqHeader *wire.MsgHeader, reqBody wire.MsgBody) (resHeader *wire.MsgHeader, resBody wire.MsgBody, closeConn bool) { //nolint:lll // argument list is too long
	var span oteltrace.Span

	var command, result, argument string
	defer func() {
		if result == "" {
			result = "panic"
		}

		if argument == "" {
			argument = "unknown"
		}

		c.m.Responses.WithLabelValues(resHeader.OpCode.String(), command, argument, result).Inc()

		must.NotBeZero(span)

		if result != "ok" {
			span.SetStatus(otelcodes.Error, result)
		}

		span.SetName(command)
		span.SetAttributes(
			otelattribute.String("db.ferretdb.opcode", resHeader.OpCode.String()),
			otelattribute.Int("db.ferretdb.request_id", int(resHeader.ResponseTo)),
			otelattribute.String("db.ferretdb.argument", argument),
		)
		span.End()
	}()

	resHeader = new(wire.MsgHeader)

	var err error
	switch reqHeader.OpCode {
	case wire.OpCodeMsg:
		msg := reqBody.(*wire.OpMsg)

		resHeader.OpCode = wire.OpCodeMsg

		var doc *wirebson.Document
		if doc, err = msg.Section0(); err == nil {
			command = doc.Command()
		}

		if err == nil {
			comment, _ := doc.Get("comment").(string)

			spanCtx, e := observability.SpanContextFromComment(comment)
			if e == nil {
				connCtx = oteltrace.ContextWithRemoteSpanContext(connCtx, spanCtx)
			} else {
				c.l.DebugContext(connCtx, "Failed to extract span context from comment", logging.Error(e))
			}
		}

		connCtx, span = otel.Tracer("").Start(connCtx, "")

		if err == nil {
			req, _ := middleware.RequestWire(reqHeader, msg)

			var resp *middleware.Response
			if resp, err = c.h.Handle(connCtx, req); err == nil {
				resBody = resp.WireBody()
			}
		}

	case wire.OpCodeQuery:
		query := reqBody.(*wire.OpQuery)

		resHeader.OpCode = wire.OpCodeReply

		var q *wirebson.Document
		if q, err = query.Query(); err == nil {
			command = q.Command()
		}

		connCtx, span = otel.Tracer("").Start(connCtx, "")

		if err == nil {
			req, _ := middleware.RequestWire(reqHeader, query)

			var resp *middleware.Response
			if resp, err = c.h.Handle(connCtx, req); err == nil {
				resBody = resp.WireBody()
			}
		}

	case wire.OpCodeReply:
		fallthrough
	case wire.OpCodeUpdate:
		fallthrough
	case wire.OpCodeInsert:
		fallthrough
	case wire.OpCodeGetByOID:
		fallthrough
	case wire.OpCodeGetMore:
		fallthrough
	case wire.OpCodeDelete:
		fallthrough
	case wire.OpCodeKillCursors:
		fallthrough
	case wire.OpCodeCompressed:
		connCtx, span = otel.Tracer("").Start(connCtx, "")
		err = lazyerrors.Errorf("unhandled OpCode %s", reqHeader.OpCode)

	default:
		connCtx, span = otel.Tracer("").Start(connCtx, "")
		err = lazyerrors.Errorf("unexpected OpCode %s", reqHeader.OpCode)
	}

	if command == "" {
		command = "unknown"
	}

	c.m.Requests.WithLabelValues(reqHeader.OpCode.String(), command).Inc()

	// set body for error
	if err != nil {
		switch resHeader.OpCode {
		case wire.OpCodeMsg:
			protoErr := mongoerrors.Make(connCtx, err, "", c.l)
			resBody = protoErr.Msg()
			result = protoErr.Name
			argument = protoErr.Argument

		case wire.OpCodeReply:
			protoErr := mongoerrors.Make(connCtx, err, "", c.l)
			resBody = protoErr.Reply()
			result = protoErr.Name
			argument = protoErr.Argument

		case wire.OpCodeQuery:
			fallthrough
		case wire.OpCodeUpdate:
			fallthrough
		case wire.OpCodeInsert:
			fallthrough
		case wire.OpCodeGetByOID:
			fallthrough
		case wire.OpCodeGetMore:
			fallthrough
		case wire.OpCodeDelete:
			fallthrough
		case wire.OpCodeKillCursors:
			fallthrough
		case wire.OpCodeCompressed:
			// do not panic to make fuzzing easier
			closeConn = true
			result = "unhandled"

			c.l.ErrorContext(
				connCtx,
				"Handler error for unhandled response opcode",
				logging.Error(err),
				slog.Any("opcode", resHeader.OpCode),
			)
			return

		default:
			// do not panic to make fuzzing easier
			closeConn = true
			result = "unexpected"

			c.l.ErrorContext(
				connCtx,
				"Handler error for unexpected response opcode",
				logging.Error(err),
				slog.Any("opcode", resHeader.OpCode),
			)
			return
		}
	}

	// Don't call MarshalBinary there. Fix header in the caller?
	// TODO https://github.com/FerretDB/FerretDB/issues/273
	b, err := resBody.MarshalBinary()
	if err != nil {
		result = ""
		panic(err)
	}
	resHeader.MessageLength = int32(wire.MsgHeaderLen + len(b))

	resHeader.RequestID = c.lastRequestID.Add(1)
	resHeader.ResponseTo = reqHeader.RequestID

	if result == "" {
		result = "ok"
	}

	return
}

// renamePartialFile takes over an open file `f` and closes it.
// It uses the given error to check if the connection was closed by the client,
// if so the given file is renamed to a name generated by hash,
// otherwise, it deletes the given file.
func (c *conn) renamePartialFile(ctx context.Context, f *os.File, h hash.Hash, err error) {
	// do not store partial files
	if !errors.Is(err, wire.ErrZeroRead) {
		_ = f.Close()
		_ = os.Remove(f.Name())

		return
	}

	// surprisingly, Sync is required before Rename on many OS/FS combinations
	if e := f.Sync(); e != nil {
		c.l.WarnContext(ctx, "Failed to sync file", logging.Error(e))
	}

	if e := f.Close(); e != nil {
		c.l.WarnContext(ctx, "Failed to close file", logging.Error(e))
	}

	fileName := hex.EncodeToString(h.Sum(nil))

	hashPath := filepath.Join(c.testRecordsDir, fileName[:2])
	if e := os.MkdirAll(hashPath, 0o777); e != nil {
		c.l.WarnContext(ctx, "Failed to make directory", logging.Error(e))
	}

	path := filepath.Join(hashPath, fileName+".bin")
	if e := os.Rename(f.Name(), path); e != nil {
		c.l.WarnContext(ctx, "Failed to rename file", logging.Error(e))
	}
}

// logResponse logs response's header and body and returns the log level that was used.
//
// The param `who` will be used in logs and should represent the type of the response,
// for example "Response" or "Proxy Response".
func (c *conn) logResponse(ctx context.Context, who string, resHeader *wire.MsgHeader, resBody wire.MsgBody, closeConn bool) slog.Level { //nolint:lll // for readability
	level := slog.LevelDebug

	if resHeader.OpCode == wire.OpCodeMsg {
		msg := resBody.(*wire.OpMsg)

		doc, _ := msg.Section0()

		var ok bool

		if doc != nil {
			switch v := doc.Get("ok").(type) {
			case float64:
				ok = v == 1
			case int32:
				ok = v == 1
			case int64:
				ok = v == 1
			}
		}

		if !ok {
			level = slog.LevelWarn
		}
	}

	if closeConn {
		level = slog.LevelError
	}

	if c.l.Enabled(ctx, level) {
		c.l.Log(ctx, level, who+" header: "+resHeader.String())
		c.l.Log(ctx, level, who+" message:\n"+resBody.StringIndent())
	}

	return level
}

// logDiff logs the diff between the response and the proxy response.
func (c *conn) logDiff(ctx context.Context, resHeader, proxyHeader *wire.MsgHeader, resBody, proxyBody wire.MsgBody, logLevel slog.Level) error { //nolint:lll // for readability
	diffHeader, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(resHeader.String()),
		FromFile: "res header",
		B:        difflib.SplitLines(proxyHeader.String()),
		ToFile:   "proxy header",
		Context:  1,
	})
	if err != nil {
		return err
	}

	// resBody can be nil if we got a message we could not handle at all, like unsupported OpQuery.
	var resBodyString, proxyBodyString string

	if resBody != nil {
		resBodyString = resBody.StringIndent()
	}

	if proxyBody != nil {
		proxyBodyString = proxyBody.StringIndent()
	}

	diffBody, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(resBodyString),
		FromFile: "res body",
		B:        difflib.SplitLines(proxyBodyString),
		ToFile:   "proxy body",
		Context:  1,
	})
	if err != nil {
		return err
	}

	if len(diffBody) > 0 {
		// the diff control lines (those with ---, +++, or @@) are created with a trailing newline
		diffBody = "\n" + strings.TrimSpace(diffBody)
	}

	c.l.Log(ctx, logLevel, "Header diff:\n"+diffHeader+"\nBody diff:"+diffBody)

	return nil
}
