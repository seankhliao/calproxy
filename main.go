package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/lestrrat-go/ical"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.seankhliao.com/usvc"
)

func main() {
	// server
	s := NewServer(os.Args)
	s.svc.Log.Error().Err(usvc.Run(usvc.SignalContext(), s)).Msg("exited")
}

type Server struct {
	// config
	url        *url.URL
	user, pass string

	rel sync.RWMutex
	res string

	// metrics
	inreqs  *prometheus.CounterVec
	outreqs prometheus.Counter

	// server
	svc *usvc.ServerSimple
}

func NewServer(args []string) *Server {
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	s := &Server{
		inreqs: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "calproxy_in_requests",
			Help: "incoming requests",
		},
			[]string{"status"},
		),
		outreqs: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_outgoing_reqs",
			Help: "outgoing requests",
		}),
		svc: usvc.NewServerSimple(usvc.NewConfig(fs)),
	}

	s.svc.Mux.Handle("/metrics", promhttp.Handler())
	s.svc.Mux.Handle("/", s)

	var ur string
	fs.StringVar(&ur, "target", os.Getenv("TARGET"), "url to redirect to")
	fs.StringVar(&s.user, "user", os.Getenv("AUTH_USER"), "user for basic auth")
	fs.StringVar(&s.pass, "pass", os.Getenv("AUTH_PASS"), "password for basic auth")
	fs.Parse(args[1:])

	var err error
	s.url, err = url.Parse(ur)
	if err != nil {
		s.svc.Log.Fatal().Err(err).Msg("parse target url")
	}

	s.svc.Log.Info().Str("target", s.url.String()).Msg("configured")
	return s
}

func (s *Server) Run() error {
	go func() {
		ctx := context.Background()
		err := s.getAll(ctx)
		if err != nil {
			s.svc.Log.Error().Err(err).Msg("init get all")
		}
		for range time.NewTicker(2 * time.Hour).C {
			err := s.getAll(ctx)
			if err != nil {
				s.svc.Log.Error().Err(err).Msg("init get all")
			}
		}
	}()
	return s.svc.Run()
}

func (s *Server) Shutdown() error {
	return s.svc.Shutdown()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	remote := r.Header.Get("x-forwarded-for")
	if remote == "" {
		remote = r.RemoteAddr
	}

	s.rel.RLock()
	defer s.rel.RUnlock()
	if s.res == "" {
		w.WriteHeader(http.StatusInternalServerError)
		s.svc.Log.Error().Str("user-agent", r.Header.Get("user-agent")).Str("remote", remote).Msg("no content")
		s.inreqs.WithLabelValues("err").Inc()
		return
	}
	w.Write([]byte(s.res))

	s.svc.Log.Info().Str("user-agent", r.Header.Get("user-agent")).Str("remote", remote).Msg("served")
	s.inreqs.WithLabelValues("ok").Inc()
}

func (s *Server) getAll(ctx context.Context) error {
	urls, err := s.getIndex(ctx)
	if err != nil {
		return fmt.Errorf("proxy.getAll: %w", err)
	}
	cal, err := s.getIcs(ctx, urls)
	if err != nil {
		return fmt.Errorf("proxy.getAll: %w", err)
	}
	s.rel.Lock()
	s.res = cal.String()
	s.rel.Unlock()
	return nil
}

func (s *Server) getIndex(ctx context.Context) ([]string, error) {
	b, err := s.get(ctx, s.url.String())
	if err != nil {
		return nil, fmt.Errorf("proxy.getIndex: %w", err)
	}

	var x HTML
	err = xml.Unmarshal(b, &x)
	if err != nil {
		return nil, fmt.Errorf("proxy.getIndex unmarshal: %w", err)
	}

	var urls []string
	for _, section := range x.Body.Section {
		if section.Table.Class != "nodeTable" {
			continue
		}
	rowloop:
		for _, row := range section.Table.Tr {
			for _, cell := range row.Td {
				if cell.Class != "nameColumn" {
					continue
				}
				urls = append(urls, cell.A.Href)
				continue rowloop
			}
		}
	}
	return urls, nil
}

func (s *Server) getIcs(ctx context.Context, urls []string) (*ical.Calendar, error) {
	var wg sync.WaitGroup
	icsec, icstc := make(chan *ical.Event, len(urls)), make(chan *ical.Timezone, 10)
	done, calc := make(chan struct{}), make(chan *ical.Calendar)

	cal := ical.NewCalendar()
	go func() {
	loop:
		for {
			select {
			case tz := <-icstc:
				cal.AddEntry(tz)
			case ev := <-icsec:
				cal.AddEntry(ev)
			case <-done:
				break loop
			}
		}
		close(icsec)
		close(icstc)
		calc <- cal
	}()

	sem := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		sem <- struct{}{}
	}

	wg.Add(len(urls))
	for _, u := range urls {
		<-sem
		go func(u string) {
			defer func() {
				wg.Done()
				sem <- struct{}{}
			}()

			URL := url.URL{
				Scheme: s.url.Scheme,
				Host:   s.url.Host,
				Path:   u,
			}
			b, err := s.get(ctx, URL.String())
			if err != nil {
				s.svc.Log.Error().Err(err).Msg("proxy.getIcs get")
				return
			}

			cal, err := ical.NewParser().Parse(bytes.NewBuffer(b))
			if err != nil {
				s.svc.Log.Error().Err(err).Msg("proxy.getIcs parse")
			}

			for e := range cal.Entries() {
				if ev, ok := e.(*ical.Event); ok {
					icsec <- ev
				} else if tz, ok := e.(*ical.Timezone); ok {
					icstc <- tz
				} else {
					s.svc.Log.Error().Str("type", e.Type()).Msg("proxy.getIcs unhandled entry")
				}

			}
		}(u)
	}

	wg.Wait()
	done <- struct{}{}

	return <-calc, nil
}

func (s *Server) get(ctx context.Context, u string) ([]byte, error) {
	s.outreqs.Inc()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy.get build request: %w", err)
	}
	req.SetBasicAuth(s.user, s.pass)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy.get do request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy.get resonse: %d %s", res.StatusCode, res.Status)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("proxy.get read body: %w", err)
	}
	return body, nil
}

type HTML struct {
	XMLName xml.Name `xml:"html"`
	Body    struct {
		Section []struct {
			Table struct {
				Class string `xml:"class,attr"`
				Tr    []struct {
					Td []struct {
						Class string `xml:"class,attr"`
						A     struct {
							Href string `xml:"href,attr"`
						} `xml:"a"`
					} `xml:"td"`
				} `xml:"tr"`
			} `xml:"table"`
		} `xml:"section"`
	} `xml:"body"`
}
