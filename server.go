package camo

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

const headerClientID = "camo-client-id"

const (
	defaultServerIfaceWriteChanLen  = 256
	defaultServerTunnelWriteChanLen = 256
	defaultSessionTTL               = time.Hour
)

var (
	// ErrNoIPConfig ...
	ErrNoIPConfig = newError(http.StatusUnprocessableEntity, "no ip config")
	// ErrIPExhausted ...
	ErrIPExhausted = newError(http.StatusServiceUnavailable, "ip exhausted")
	// ErrIPConflict ...
	ErrIPConflict = newError(http.StatusConflict, "ip conflict")
	// ErrInvalidIP ...
	ErrInvalidIP = newError(http.StatusBadRequest, "invalid ip address")
)

// Server ...
type Server struct {
	MTU        int
	IPv4Pool   IPPool
	IPv6Pool   IPPool
	SessionTTL time.Duration
	Logger     Logger

	mu             sync.RWMutex
	ipSession      map[string]*session
	cidIPv4Session map[string]*session
	cidIPv6Session map[string]*session

	bufPool        sync.Pool
	ifaceWriteChan chan []byte
	doneChan       chan struct{}

	metrics     *Metrics
	metricsOnce sync.Once
}

func (s *Server) getIfaceWriteChan() chan []byte {
	s.mu.RLock()
	if s.ifaceWriteChan != nil {
		s.mu.RUnlock()
		return s.ifaceWriteChan
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ifaceWriteChan == nil {
		s.ifaceWriteChan = make(chan []byte, defaultServerIfaceWriteChanLen)
	}
	return s.ifaceWriteChan
}

func (s *Server) getDoneChan() chan struct{} {
	s.mu.RLock()
	if s.doneChan != nil {
		s.mu.RUnlock()
		return s.doneChan
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doneChan == nil {
		s.doneChan = make(chan struct{})
	}
	return s.doneChan
}

func (s *Server) mtu() int {
	if s.MTU <= 0 {
		return DefaultMTU
	}
	return s.MTU
}

func (s *Server) getBuffer() []byte {
	b := s.bufPool.Get()
	if b != nil {
		return b.([]byte)
	}
	buf := make([]byte, s.mtu())
	s.Metrics().BufferSize.Add(int64(len(buf)))
	return buf
}

func (s *Server) freeBuffer(b []byte) {
	s.bufPool.Put(b[:cap(b)])
}

func (s *Server) logger() Logger {
	if s.Logger == nil {
		return (*LevelLogger)(nil)
	}
	return s.Logger
}

// Metrics ...
func (s *Server) Metrics() *Metrics {
	s.metricsOnce.Do(func() {
		s.metrics = NewMetrics()
	})
	return s.metrics
}

// Serve ...
func (s *Server) Serve(iface io.ReadWriteCloser) error {
	var (
		log     = s.logger()
		metrics = s.Metrics()
		rw      = WithIOMetric(iface, metrics.Iface)
		h       ipv4.Header
	)
	return serveIO(s.getDoneChan(), rw, s, func(_ <-chan struct{}, pkt []byte) (ok bool) {
		if e := parseIPv4Header(&h, pkt); e != nil {
			log.Warn("iface failed to parse ipv4 header:", e)
			return
		}
		if h.Version != 4 {
			log.Tracef("iface drop ip version %d", h.Version)
			return
		}
		log.Tracef("iface recv: %s", &h)
		ss, ok := s.getSession(h.Dst)
		if !ok {
			log.Debugf("iface drop packet to %s: missing session", h.Dst)
			return
		}
		select {
		case ss.writeChan <- pkt:
			ok = true
			ss.lags.Add(1)
			metrics.Tunnels.Lags.Add(1)
			return
		default:
			metrics.Tunnels.Drops.Add(1)
			log.Debugf("iface drop packet to %s: channel full", h.Dst)
			return
		}
	}, s.getIfaceWriteChan(), nil)
}

// Close ...
func (s *Server) Close() {
	done := s.getDoneChan()
	select {
	case <-done:
	default:
		close(done)
	}
}

func (s *Server) sessionTTL() time.Duration {
	if s.SessionTTL == 0 {
		return defaultSessionTTL
	}
	return s.SessionTTL
}

func (s *Server) createSessionLocked(ip net.IP, cid string) *session {
	if s.ipSession == nil {
		s.ipSession = make(map[string]*session)
		s.cidIPv4Session = make(map[string]*session)
		s.cidIPv6Session = make(map[string]*session)
	}

	log := s.logger()

	ss := &session{
		cid:        cid,
		ip:         ip,
		createTime: time.Now(),
		writeChan:  make(chan []byte, defaultServerTunnelWriteChanLen),
	}

	startTimer := func() *time.Timer {
		return time.AfterFunc(s.sessionTTL(), func() {
			if ss.setDone() {
				s.removeSession(ss.ip)
				log.Debugf("session %s expired", ss.ip)
			}
		})
	}
	timer := startTimer()
	ss.onRetained = func() {
		timer.Stop()
	}
	ss.onReleased = func() {
		timer = startTimer()
	}

	s.ipSession[ip.String()] = ss
	if ip.To4() != nil {
		s.cidIPv4Session[cid] = ss
	} else {
		s.cidIPv6Session[cid] = ss
	}

	return ss
}

func (s *Server) getSession(ip net.IP) (*session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.ipSession[ip.String()]
	return ss, ok
}

func (s *Server) getOrCreateSession(ip net.IP, cid string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ss, ok := s.ipSession[ip.String()]
	if ok {
		if ss.cid != cid {
			return nil, ErrIPConflict
		}
		return ss, nil
	}

	var ippool IPPool
	if ip.To4() != nil {
		ippool = s.IPv4Pool
	} else {
		ippool = s.IPv6Pool
	}
	if ippool == nil {
		return nil, ErrNoIPConfig
	}
	if !ippool.Use(ip, cid) {
		return nil, ErrInvalidIP
	}

	return s.createSessionLocked(ip, cid), nil
}

func (s *Server) removeSession(ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.ipSession[ip.String()]
	if ok {
		delete(s.ipSession, ip.String())
		if ip.To4() != nil {
			delete(s.cidIPv4Session, ss.cid)
		} else {
			delete(s.cidIPv6Session, ss.cid)
		}
		s.Metrics().Tunnels.Lags.Add(-ss.lags.Value())
	}
}

// RequestIPv4 ...
func (s *Server) RequestIPv4(cid string) (ip net.IP, ttl time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	getTTL := func(ttl time.Duration, idle time.Duration) time.Duration {
		d := ttl - idle
		if d < 0 {
			return 0
		}
		return d
	}

	ss, ok := s.cidIPv4Session[cid]
	if ok {
		return ss.ip, getTTL(s.sessionTTL(), ss.idleDuration()), nil
	}

	if s.IPv4Pool == nil {
		err = ErrNoIPConfig
		return
	}

	ip, ok = s.IPv4Pool.Get(cid)
	if !ok {
		err = ErrIPExhausted
		return
	}

	ss = s.createSessionLocked(ip, cid)
	return ip, s.sessionTTL(), nil
}

// OpenTunnel ...
func (s *Server) OpenTunnel(ip net.IP, cid string, rw io.ReadWriteCloser) (func(stop <-chan struct{}) error, error) {
	ss, err := s.getOrCreateSession(ip, cid)
	if err != nil {
		return nil, err
	}

	return func(stop <-chan struct{}) error {
		if !ss.retain() {
			return newError(http.StatusUnprocessableEntity, "session expired")
		}
		defer ss.release()

		metrics := s.Metrics()
		metrics.Tunnels.Streams.Add(1)
		defer metrics.Tunnels.Streams.Add(-1)

		rw = WithIOMetric(&packetIO{rw}, metrics.Tunnels.IOMetric)
		postWrite := func(<-chan struct{}, error) {
			ss.lags.Add(-1)
			metrics.Tunnels.Lags.Add(-1)
		}

		var (
			log        = s.logger()
			ifaceWrite = s.getIfaceWriteChan()
			h          ipv4.Header
		)
		return serveIO(stop, rw, s, func(stop <-chan struct{}, pkt []byte) (ok bool) {
			err = parseIPv4Header(&h, pkt)
			if err != nil {
				log.Warn("tunnel failed to parse ipv4 header:", err)
				return
			}
			log.Tracef("tunnel recv: %s", &h)
			if !h.Src.Equal(ss.ip) {
				log.Warnf("tunnel drop packet from %s: src (%s) mismatched", ss.ip, h.Src)
				return
			}
			select {
			case ifaceWrite <- pkt:
				ok = true
				return
			case <-stop:
				s.freeBuffer(pkt)
				return
			}
		}, ss.writeChan, postWrite)
	}, nil
}

// Handler ...
func (s *Server) Handler(prefix string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/ip/v4", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		cid := r.Header.Get(headerClientID)
		if cid == "" {
			http.Error(w, "missing "+headerClientID, http.StatusBadRequest)
			return
		}

		ip, ttl, err := s.RequestIPv4(cid)
		if err != nil {
			http.Error(w, err.Error(), getStatusCode(err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(&struct {
			IP  string `json:"ip"`
			TTL int    `json:"ttl"`
		}{
			IP:  ip.String(),
			TTL: int(ttl / time.Second),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	mux.HandleFunc(prefix+"/tunnel/", func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2.0 required", http.StatusUpgradeRequired)
			return
		}

		argIP := strings.TrimPrefix(r.URL.Path, prefix+"/tunnel/")
		if strings.Contains(argIP, "/") {
			http.NotFound(w, r)
			return
		}
		ip := net.ParseIP(argIP)
		if ip == nil {
			http.Error(w, "invalid ip address", http.StatusBadRequest)
			return
		}
		ip = ip.To4()
		if ip == nil {
			http.Error(w, "ipv4 address required", http.StatusBadRequest)
			return
		}

		if r.Method != "POST" {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		cid := r.Header.Get(headerClientID)
		if cid == "" {
			http.Error(w, "missing header: "+headerClientID, http.StatusBadRequest)
			return
		}

		tunnel, err := s.OpenTunnel(ip, cid, &httpServerStream{r.Body, w})
		if err != nil {
			http.Error(w, err.Error(), getStatusCode(err))
			return
		}

		// flush the header frame
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		log := s.logger()
		log.Infof("tunnel %s opened, cid: %s, remote: %s", ip, cid, r.RemoteAddr)

		err = tunnel(s.getDoneChan())
		if err != nil {
			log.Infof("tunnel %s closed. %v", ip, err)
		} else {
			log.Infof("tunnel %s closed", ip)
		}
	})

	return mux
}

type httpServerStream struct {
	io.ReadCloser
	w io.Writer
}

func (s *httpServerStream) Write(b []byte) (int, error) {
	n, err := s.w.Write(b)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
