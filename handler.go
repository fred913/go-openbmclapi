/**
 * OpenBmclAPI (Golang Edition)
 * Copyright (C) 2023 Kevin Z <zyxkad@gmail.com>
 * All rights reserved
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Affero General Public License as published
 *  by the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU Affero General Public License for more details.
 *
 *  You should have received a copy of the GNU Affero General Public License
 *  along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LiterMC/go-openbmclapi/log"
	"github.com/LiterMC/go-openbmclapi/storage"
	"github.com/LiterMC/go-openbmclapi/utils"
)

func init() {
	// ignore TLS handshake error
	log.AddStdLogFilter(func(line []byte) bool {
		return bytes.HasPrefix(line, ([]byte)("http: TLS handshake error"))
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  int64
}

var _ http.Hijacker = (*statusResponseWriter)(nil)

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(buf []byte) (n int, err error) {
	n, err = w.ResponseWriter.Write(buf)
	w.wrote += (int64)(n)
	return
}

func (w *statusResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("ResponseWriter is not http.Hijacker")
}

const (
	RealAddrCtxKey       = "handle.real.addr"
	RealPathCtxKey       = "handle.real.path"
	AccessLogExtraCtxKey = "handle.access.extra"
)

func GetRequestRealPath(req *http.Request) string {
	return req.Context().Value(RealPathCtxKey).(string)
}

func SetAccessInfo(req *http.Request, key string, value any) {
	if info, ok := req.Context().Value(AccessLogExtraCtxKey).(map[string]any); ok {
		info[key] = value
	}
}

type preAccessRecord struct {
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Addr   string    `json:"addr"`
	Method string    `json:"method"`
	URI    string    `json:"uri"`
	UA     string    `json:"ua"`
}

func (r *preAccessRecord) String() string {
	return fmt.Sprintf("Serving %-15s | %-4s %s | %q", r.Addr, r.Method, r.URI, r.UA)
}

type accessRecord struct {
	Type    string         `json:"type"`
	Status  int            `json:"status"`
	Used    time.Duration  `json:"used"`
	Content int64          `json:"content"`
	Addr    string         `json:"addr"`
	Proto   string         `json:"proto"`
	Method  string         `json:"method"`
	URI     string         `json:"uri"`
	UA      string         `json:"ua"`
	Extra   map[string]any `json:"extra,omitempty"`
}

func (r *accessRecord) String() string {
	used := r.Used
	if used > time.Minute {
		used = used.Truncate(time.Second)
	} else if used > time.Second {
		used = used.Truncate(time.Microsecond)
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, "Serve %3d | %12v | %7s | %-15s | %s | %-4s %s | %q",
		r.Status, used, bytesToUnit((float64)(r.Content)),
		r.Addr, r.Proto,
		r.Method, r.URI, r.UA)
	if len(r.Extra) > 0 {
		buf.WriteString(" | ")
		e := json.NewEncoder(&utils.NoLastNewLineWriter{Writer: &buf})
		e.SetEscapeHTML(false)
		e.Encode(r.Extra)
	}
	return buf.String()
}

func (cr *Cluster) GetHandler() http.Handler {
	cr.handlerAPIv0 = http.StripPrefix("/api/v0", cr.cliIdHandle(cr.initAPIv0()))
	cr.hijackHandler = http.StripPrefix("/bmclapi", cr.hijackProxy)

	handler := NewHttpMiddleWareHandler(cr)
	// recover panic and log it
	handler.Use(func(rw http.ResponseWriter, req *http.Request, next http.Handler) {
		defer log.RecoverPanic(func(any) {
			rw.WriteHeader(http.StatusInternalServerError)
		})
		next.ServeHTTP(rw, req)
	})

	if !config.Advanced.DoNotRedirectHTTPSToSecureHostname {
		// rediect the client to the first public host if it is connecting with a unsecure host
		handler.Use(func(rw http.ResponseWriter, req *http.Request, next http.Handler) {
			host, _, err := net.SplitHostPort(req.Host)
			if err != nil {
				host = req.Host
			}
			if host != "" && len(cr.publicHosts) > 0 {
				host = strings.ToLower(host)
				ok := false
				for _, h := range cr.publicHosts { // cr.publicHosts are already lower case
					if host == h {
						ok = true
						break
					}
				}
				if !ok {
					u := *req.URL
					u.Scheme = "https"
					u.Host = net.JoinHostPort(cr.publicHosts[0], strconv.Itoa((int)(cr.publicPort)))
					rw.Header().Set("Location", u.String())
					rw.Header().Set("Content-Length", "0")
					rw.WriteHeader(http.StatusFound)
					return
				}
			}
			next.ServeHTTP(rw, req)
		})
	}

	handler.Use(cr.getRecordMiddleWare())
	return handler
}

func (cr *Cluster) getRecordMiddleWare() MiddleWareFunc {
	type record struct {
		used    float64
		bytes   float64
		ua      string
		isRange bool
	}
	recordCh := make(chan record, 1024)

	go func() {
		<-cr.WaitForEnable()
		disabled := cr.Disabled()

		updateTicker := time.NewTicker(time.Minute)
		defer updateTicker.Stop()

		var (
			total      int
			totalUsed  float64
			totalBytes float64
			uas        = make(map[string]int, 10)
		)
		for {
			select {
			case <-updateTicker.C:
				cr.stats.mux.Lock()

				log.Infof("Served %d requests, total responsed body = %s, total used CPU time = %.2fs",
					total, bytesToUnit(totalBytes), totalUsed)
				for ua, v := range uas {
					if ua == "" {
						ua = "[Unknown]"
					}
					cr.stats.Accesses[ua] += v
				}

				total = 0
				totalUsed = 0
				totalBytes = 0
				clear(uas)

				cr.stats.mux.Unlock()
			case rec := <-recordCh:
				total++
				totalUsed += rec.used
				totalBytes += rec.bytes
				if !rec.isRange {
					uas[rec.ua]++
				}
			case <-disabled:
				total = 0
				totalUsed = 0
				totalBytes = 0
				clear(uas)

				select {
				case <-cr.WaitForEnable():
					disabled = cr.Disabled()
				case <-time.After(time.Hour):
					return
				}
			}
		}
	}()

	return func(rw http.ResponseWriter, req *http.Request, next http.Handler) {
		ua := req.UserAgent()
		var addr string
		if config.TrustedXForwardedFor {
			// X-Forwarded-For: <client>, <proxy1>, <proxy2>
			adr, _, _ := strings.Cut(req.Header.Get("X-Forwarded-For"), ",")
			addr = strings.TrimSpace(adr)
		}
		if addr == "" {
			addr, _, _ = net.SplitHostPort(req.RemoteAddr)
		}
		srw := &statusResponseWriter{ResponseWriter: rw}
		start := time.Now()

		log.LogAccess(log.LevelDebug, &preAccessRecord{
			Type:   "pre-access",
			Time:   start,
			Addr:   addr,
			Method: req.Method,
			URI:    req.RequestURI,
			UA:     ua,
		})

		extraInfoMap := make(map[string]any)
		ctx := req.Context()
		ctx = context.WithValue(ctx, RealAddrCtxKey, addr)
		ctx = context.WithValue(ctx, RealPathCtxKey, req.URL.Path)
		ctx = context.WithValue(ctx, AccessLogExtraCtxKey, extraInfoMap)
		req = req.WithContext(ctx)
		next.ServeHTTP(srw, req)

		used := time.Since(start)
		accRec := &accessRecord{
			Type:    "access",
			Status:  srw.status,
			Used:    used,
			Content: srw.wrote,
			Addr:    addr,
			Proto:   req.Proto,
			Method:  req.Method,
			URI:     req.RequestURI,
			UA:      ua,
		}
		if len(extraInfoMap) > 0 {
			accRec.Extra = extraInfoMap
		}
		log.LogAccess(log.LevelInfo, accRec)

		if srw.status < 200 && 400 <= srw.status {
			return
		}
		if !strings.HasPrefix(req.URL.Path, "/download/") {
			return
		}
		var rec record
		rec.used = used.Seconds()
		rec.bytes = (float64)(srw.wrote)
		ua, _, _ = strings.Cut(ua, " ")
		rec.ua, _, _ = strings.Cut(ua, "/")
		rec.isRange = extraInfoMap["skip-ua-count"] != nil
		select {
		case recordCh <- rec:
		default:
		}
	}
}

var emptyHashes = func() (hashes map[string]struct{}) {
	hashMethods := []crypto.Hash{
		crypto.MD5, crypto.SHA1,
	}
	hashes = make(map[string]struct{}, len(hashMethods))
	for _, h := range hashMethods {
		hs := hex.EncodeToString(h.New().Sum(nil))
		hashes[hs] = struct{}{}
	}
	return
}()

const HeaderXPoweredBy = "go-openbmclapi; url=https://github.com/LiterMC/go-openbmclapi"

func (cr *Cluster) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	method := req.Method
	u := req.URL

	rw.Header().Set("X-Powered-By", HeaderXPoweredBy)

	rawpath := u.EscapedPath()
	switch {
	case strings.HasPrefix(rawpath, "/download/"):
		if method != http.MethodGet && method != http.MethodHead {
			rw.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			http.Error(rw, "405 Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		hash := rawpath[len("/download/"):]
		if !utils.IsHex(hash) {
			http.Error(rw, hash+" is not a valid hash", http.StatusNotFound)
			return
		}

		query := req.URL.Query()
		if !checkQuerySign(hash, cr.clusterSecret, query) {
			http.Error(rw, "Cannot verify signature", http.StatusForbidden)
			return
		}

		log.Debugf("Handling download %s", hash)
		cr.handleDownload(rw, req, hash)
		return
	case strings.HasPrefix(rawpath, "/measure/"):
		if method != http.MethodGet && method != http.MethodHead {
			rw.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			http.Error(rw, "405 Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		query := req.URL.Query()
		if !checkQuerySign(u.Path, cr.clusterSecret, query) {
			http.Error(rw, "Cannot verify signature", http.StatusForbidden)
			return
		}

		size := rawpath[len("/measure/"):]
		n, e := strconv.Atoi(size)
		if e != nil {
			http.Error(rw, e.Error(), http.StatusBadRequest)
			return
		} else if n < 0 || n > 200 {
			http.Error(rw, fmt.Sprintf("measure size %d out of range (0, 200]", n), http.StatusBadRequest)
			return
		}
		if err := cr.storages[0].ServeMeasure(rw, req, n); err != nil {
			log.Errorf("Could not serve measure %d: %v", n, err)
			http.Error(rw, err.Error(), http.StatusInternalServerError)
		}
		return
	case strings.HasPrefix(rawpath, "/api/"):
		version, _, _ := strings.Cut(rawpath[len("/api/"):], "/")
		switch version {
		case "v0":
			cr.handlerAPIv0.ServeHTTP(rw, req)
			return
		}
	case strings.HasPrefix(rawpath, "/dashboard/"):
		if !config.Dashboard.Enable {
			http.NotFound(rw, req)
			return
		}
		pth := rawpath[len("/dashboard/"):]
		cr.serveDashboard(rw, req, pth)
		return
	case rawpath == "/" || rawpath == "/dashboard":
		http.Redirect(rw, req, "/dashboard/", http.StatusFound)
		return
	case strings.HasPrefix(rawpath, "/bmclapi/"):
		cr.hijackHandler.ServeHTTP(rw, req)
		return
	}
	http.NotFound(rw, req)
}

func (cr *Cluster) handleDownload(rw http.ResponseWriter, req *http.Request, hash string) {
	keepaliveRec := req.Context().Value("go-openbmclapi.handler.no.record.for.keepalive") != true
	rw.Header().Set("X-Bmclapi-Hash", hash)

	if _, ok := emptyHashes[hash]; ok {
		name := req.URL.Query().Get("name")
		rw.Header().Set("ETag", `"`+hash+`"`)
		rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // cache for a year
		rw.Header().Set("Content-Type", "application/octet-stream")
		rw.Header().Set("Content-Length", "0")
		if name != "" {
			rw.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		}
		rw.WriteHeader(http.StatusOK)
		if keepaliveRec {
			cr.hits.Add(1)
			// cr.hbts.Add(0) // no need to add zero
		} else {
			cr.statHits.Add(1)
		}
		return
	}

	if !cr.shouldEnable.Load() {
		// do not serve file if cluster is not enabled yet
		http.Error(rw, "Cluster is not enabled yet", http.StatusServiceUnavailable)
		return
	}

	if r := req.Header.Get("Range"); r != "" {
		if start, ok := parseRangeFirstStart(r); ok && start != 0 {
			SetAccessInfo(req, "skip-ua-count", "range")
		}
	}

	var err error
	// check if file was indexed in the fileset
	size, ok := cr.CachedFileSize(hash)
	if !ok {
		if err := cr.DownloadFile(req.Context(), hash); err != nil {
			http.Error(rw, "404 not found", http.StatusNotFound)
			return
		}
	}
	var sto storage.Storage
	forEachFromRandomIndexWithPossibility(cr.storageWeights, cr.storageTotalWeight, func(i int) bool {
		sto = cr.storages[i]
		log.Debugf("[handler]: Checking %s on storage [%d] %s ...", hash, i, sto.String())

		sz, er := sto.ServeDownload(rw, req, hash, size)
		if er != nil {
			log.Debugf("[handler]: File %s failed on storage [%d] %s: %v", hash, i, sto.String(), er)
			err = er
			return false
		}
		if sz >= 0 {
			if keepaliveRec {
				cr.hits.Add(1)
				cr.hbts.Add(sz)
			} else {
				cr.statHits.Add(1)
				cr.statHbts.Add(sz)
			}
		}
		return true
	})
	if sto != nil {
		SetAccessInfo(req, "storage", sto.String())
	}
	if err != nil {
		log.Debugf("[handler]: failed to serve download: %v", err)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(rw, "404 Status Not Found", http.StatusNotFound)
			return
		}
		if _, ok := err.(*utils.HTTPStatusError); ok {
			http.Error(rw, err.Error(), http.StatusBadGateway)
		} else {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	log.Debug("[handler]: download served successed")
}

// Note: this method is a fast parse, it does not deeply check if the range is valid or not
func parseRangeFirstStart(rg string) (start int64, ok bool) {
	const b = "bytes="
	if rg, ok = strings.CutPrefix(rg, b); !ok {
		return
	}
	rg, _, _ = strings.Cut(rg, ",")
	if rg, _, ok = strings.Cut(rg, "-"); !ok {
		return
	}
	if rg = textproto.TrimString(rg); rg == "" {
		return -1, true
	}
	start, err := strconv.ParseInt(rg, 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}

type MiddleWareFunc func(rw http.ResponseWriter, req *http.Request, next http.Handler)

type httpMiddleWareHandler struct {
	final   http.Handler
	middles []MiddleWareFunc
}

func NewHttpMiddleWareHandler(final http.Handler) *httpMiddleWareHandler {
	return &httpMiddleWareHandler{
		final: final,
	}
}

func (m *httpMiddleWareHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	i := 0
	var getNext func() http.Handler
	getNext = func() http.Handler {
		j := i
		if j > len(m.middles) {
			// unreachable
			panic("httpMiddleWareHandler: called getNext too much times")
		}
		i++
		if j == len(m.middles) {
			return m.final
		}
		mid := m.middles[j]

		called := false
		return (http.HandlerFunc)(func(rw http.ResponseWriter, req *http.Request) {
			if called {
				panic("httpMiddleWareHandler: Called next function twice")
			}
			called = true
			mid(rw, req, getNext())
		})
	}
	getNext().ServeHTTP(rw, req)
}

func (m *httpMiddleWareHandler) Use(fns ...MiddleWareFunc) {
	m.middles = append(m.middles, fns...)
}
