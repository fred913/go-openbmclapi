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
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const mbChunkSize = 1024 * 1024

var mbChunk [mbChunkSize]byte

var closedCh = func() <-chan struct{} {
	ch := make(chan struct{}, 0)
	close(ch)
	return ch
}()

func split(str string, b byte) (l, r string) {
	i := strings.IndexByte(str, b)
	if i >= 0 {
		return str[:i], str[i+1:]
	}
	return str, ""
}

func splitCSV(line string) (values map[string]float32) {
	list := strings.Split(line, ",")
	values = make(map[string]float32, len(list))
	for _, v := range list {
		name, opt := split(strings.ToLower(strings.TrimSpace(v)), ';')
		var q float64 = 1
		if v, ok := strings.CutPrefix(opt, "q="); ok {
			q, _ = strconv.ParseFloat(v, 32)
		}
		values[name] = (float32)(q)
	}
	return
}

func createInterval(ctx context.Context, do func(), delay time.Duration) {
	logDebug("Interval created:", ctx)
	go func() {
		ticker := time.NewTicker(delay)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logDebug("Interval stopped:", ctx)
				return
			case <-ticker.C:
				do()
				// If there is a tick passed during the job, ignore it
				select {
				case <-ticker.C:
				default:
				}
			}
		}
	}()
	return
}

func httpToWs(origin string) string {
	if strings.HasPrefix(origin, "http") {
		return "ws" + origin[4:]
	}
	return origin
}

func bytesToUnit(size float64) string {
	if size < 1000 {
		return fmt.Sprintf("%dB", (int)(size))
	}
	size /= 1024
	unit := "KB"
	if size >= 1000 {
		size /= 1024
		unit = "MB"
		if size >= 1000 {
			size /= 1024
			unit = "GB"
			if size >= 1000 {
				size /= 1024
				unit = "TB"
			}
		}
	}
	return fmt.Sprintf("%.1f%s", size, unit)
}

func withContext(ctx context.Context, call func()) bool {
	if ctx == nil {
		call()
		return true
	}
	done := make(chan struct{}, 0)
	go func() {
		defer close(done)
		call()
	}()
	select {
	case <-ctx.Done():
		return false
	case <-done:
		return true
	}
}

const BUF_SIZE = 1024 * 512 // 512KB
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, BUF_SIZE)
		return &buf
	},
}

func getHashMethod(l int) (hashMethod crypto.Hash, err error) {
	switch l {
	case 32:
		hashMethod = crypto.MD5
	case 40:
		hashMethod = crypto.SHA1
	default:
		err = fmt.Errorf("Unknown hash length %d", l)
	}
	return
}

func parseCertCommonName(cert []byte) (string, error) {
	rest := cert
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return "", nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		return cert.Subject.CommonName, nil
	}
}

var rd = func() chan int {
	ch := make(chan int, 64)
	r := rand.New(rand.NewSource(time.Now().Unix()))
	go func() {
		for {
			ch <- r.Int()
		}
	}()
	return ch
}()

func randIntn(n int) int {
	rn := <-rd
	return rn % n
}

func forEachFromRandomIndex(leng int, cb func(i int) (done bool)) (done bool) {
	if leng <= 0 {
		return false
	}
	start := randIntn(leng)
	for i := start; i < leng; i++ {
		if cb(i) {
			return true
		}
	}
	for i := 0; i < start; i++ {
		if cb(i) {
			return true
		}
	}
	return false
}

func forEachFromRandomIndexWithPossibility(poss []uint, total uint, cb func(i int) (done bool)) (done bool) {
	leng := len(poss)
	if leng == 0 {
		return false
	}
	if total == 0 {
		return forEachFromRandomIndex(leng, cb)
	}
	n := (uint)(randIntn((int)(total)))
	start := 0
	for i, p := range poss {
		if n < p {
			start = i
			break
		}
		n -= p
	}
	for i := start; i < leng; i++ {
		if cb(i) {
			return true
		}
	}
	for i := 0; i < start; i++ {
		if cb(i) {
			return true
		}
	}
	return false
}

func copyFile(src, dst string, mode os.FileMode) (err error) {
	var srcFd, dstFd *os.File
	if srcFd, err = os.Open(src); err != nil {
		return
	}
	defer srcFd.Close()
	if dstFd, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode); err != nil {
		return
	}
	defer dstFd.Close()
	_, err = io.Copy(dstFd, srcFd)
	return
}

type devNull struct{}

var (
	DevNull = devNull{}

	_ io.ReaderAt   = DevNull
	_ io.ReadSeeker = DevNull
	_ io.Writer     = DevNull
)

func (devNull) Read([]byte) (int, error)          { return 0, io.EOF }
func (devNull) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }
func (devNull) Seek(int64, int) (int64, error)    { return 0, nil }
func (devNull) Write(buf []byte) (int, error)     { return len(buf), nil }

type emptyReader struct{}

var (
	EmptyReader = emptyReader{}

	_ io.ReaderAt = EmptyReader
)

func (emptyReader) ReadAt(buf []byte, _ int64) (int, error) { return len(buf), nil }

var errNotSeeker = errors.New("r is not an io.Seeker")

func getFileSize(r io.Reader) (n int64, err error) {
	if s, ok := r.(io.Seeker); ok {
		if n, err = s.Seek(0, io.SeekEnd); err == nil {
			if _, err = s.Seek(0, io.SeekStart); err != nil {
				return
			}
		}
	} else {
		err = errNotSeeker
	}
	return
}

func checkQuerySign(hash string, secret string, query url.Values) bool {
	sign, e := query.Get("s"), query.Get("e")
	if len(sign) == 0 || len(e) == 0 {
		return false
	}
	before, err := strconv.ParseInt(e, 36, 64)
	if err != nil {
		return false
	}
	hs := crypto.SHA1.New()
	io.WriteString(hs, secret)
	io.WriteString(hs, hash)
	io.WriteString(hs, e)
	var (
		buf  [20]byte
		sbuf [27]byte
	)
	base64.RawURLEncoding.Encode(sbuf[:], hs.Sum(buf[:0]))
	if (string)(sbuf[:]) != sign {
		return false
	}
	return time.Now().UnixMilli() < before
}

func initCache(base string) (err error) {
	if err = os.MkdirAll(base, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return
	}
	var b [1]byte
	for i := 0; i < 0x100; i++ {
		b[0] = (byte)(i)
		if err = os.Mkdir(filepath.Join(base, hex.EncodeToString(b[:])), 0755); err != nil && !errors.Is(err, os.ErrExist) {
			return
		}
	}
	return nil
}

type SyncMap[K comparable, V any] struct {
	l sync.RWMutex
	m map[K]V
}

func NewSyncMap[K comparable, V any]() *SyncMap[K, V] {
	return &SyncMap[K, V]{
		m: make(map[K]V),
	}
}

func (m *SyncMap[K, V]) Len() int {
	m.l.RLock()
	defer m.l.RUnlock()
	return len(m.m)
}

func (m *SyncMap[K, V]) Set(k K, v V) {
	m.l.Lock()
	defer m.l.Unlock()
	m.m[k] = v
}

func (m *SyncMap[K, V]) Get(k K) V {
	m.l.RLock()
	defer m.l.RUnlock()
	return m.m[k]
}

func (m *SyncMap[K, V]) Has(k K) bool {
	m.l.RLock()
	defer m.l.RUnlock()
	_, ok := m.m[k]
	return ok
}

func (m *SyncMap[K, V]) GetOrSet(k K, setter func() V) (v V, has bool) {
	m.l.RLock()
	v, has = m.m[k]
	m.l.RUnlock()
	if has {
		return
	}
	m.l.Lock()
	defer m.l.Unlock()
	v, has = m.m[k]
	if !has {
		v = setter()
		m.m[k] = v
	}
	return
}

type HTTPStatusError struct {
	Code int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("Unexpected http status %d %s", e.Code, http.StatusText(e.Code))
}

type RawYAML struct {
	*yaml.Node
}

var (
	_ yaml.Marshaler   = RawYAML{}
	_ yaml.Unmarshaler = (*RawYAML)(nil)
)

func (r RawYAML) MarshalYAML() (any, error) {
	return r.Node, nil
}

func (r *RawYAML) UnmarshalYAML(n *yaml.Node) (err error) {
	r.Node = n
	return nil
}

type YAMLDuration time.Duration

func (d YAMLDuration) Dur() time.Duration {
	return (time.Duration)(d)
}

func (d YAMLDuration) MarshalYAML() (any, error) {
	return (time.Duration)(d).String(), nil
}

func (d *YAMLDuration) UnmarshalYAML(n *yaml.Node) (err error) {
	var v string
	if err = n.Decode(v); err != nil {
		return
	}
	var td time.Duration
	if td, err = time.ParseDuration(v); err != nil {
		return
	}
	*d = (YAMLDuration)(td)
	return nil
}

type slotInfo struct {
	id  int
	buf []byte
}

type BufSlots struct {
	c chan slotInfo
}

func NewBufSlots(size int) *BufSlots {
	c := make(chan slotInfo, size)
	for i := 0; i < size; i++ {
		c <- slotInfo{
			id:  i,
			buf: make([]byte, 1024*512),
		}
	}
	return &BufSlots{
		c: c,
	}
}

func (s *BufSlots) Len() int {
	return len(s.c)
}

func (s *BufSlots) Cap() int {
	return cap(s.c)
}

func (s *BufSlots) Alloc(ctx context.Context) (slotId int, buf []byte, free func()) {
	select {
	case slot := <-s.c:
		return slot.id, slot.buf, func() {
			s.c <- slot
		}
	case <-ctx.Done():
		return 0, nil, nil
	}
}
