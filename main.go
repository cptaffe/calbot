package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/cptaffe/blog/logging"
	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/net/html"
)

var (
	eventPattern = regexp.MustCompile(`^(Thursday|Friday|Saturday|Sunday)([ 	]*(-|–|&) (Thursday|Friday|Saturday|Sunday))?,[ 	]*(January|February|March|April|May|June|July|August|September|October|November|December)[ 	]*([1-9][0-9]*)([ 	]*(-|–|&)[ 	]*?([1-9][0-9]*))?$`)
	timePattern  = regexp.MustCompile(`([1-9][0-2]*(:[0-6][0-9])?([ 	]*((a|p).m.)?[ 	]*(-|–)[ 	]*([1-9][0-2]*(:[0-6][0-9])?))?[ 	]+(a|p).m.)|([1-9][0-2]*(:[0-6][0-9])?([ 	]*((a|p).m.)?[ 	]*(-|–)[ 	]*([1-9][0-2]*(:[0-6][0-9])?))?[ 	]+(a|p).m.)([ 	]+on)?[ 	]+(Thursday|Friday|Saturday|Sunday)|(Thursday|Friday|Saturday|Sunday).*at[ 	]+(([1-9][0-2]*(:[0-6][0-9])? (a|p).m.))`)

	singleDayTimePattern = regexp.MustCompile(`(([1-9][0-2]*(:[0-6][0-9])?)([ 	]*((a|p)\.?m\.?)?[ 	]*(-|–)[ 	]*([1-9][0-2]*(:[0-6][0-9])?))?[ 	]+(a|p)\.?m\.?)`)
	linkTextPattern      = regexp.MustCompile(`Learn more here.?`)
	locationTextPattern  = regexp.MustCompile(`(.*) (at|in) (the )?(.*)`)

	templatesPath = flag.String("templates", "", "path to templates")
	address       = flag.String("address", ":8080", "address to listen on, :8080 by default")

	logger = log.Default()
)

type DateRange struct {
	Start time.Time
	End   time.Time
}

type TimeRange struct {
	Start time.Time
	End   time.Time
}

type Event struct {
	Dates       DateRange
	Times       []TimeRange
	Title       string
	Body        strings.Builder
	Description string
	Link        string
	Location    string
}

type Parser struct {
	time      time.Time
	tokenizer *html.Tokenizer
	tt        *html.TokenType
	tn        string
	headerTag string
	dates     DateRange
	event     *Event
	Events    chan *Event
}

func NewParser(time time.Time, r io.Reader) *Parser {
	return &Parser{time: time, tokenizer: html.NewTokenizer(r), Events: make(chan *Event)}
}

func (p *Parser) Run() {
	for f := p.ParseRoot; f != nil; f = f() {
	}
	close(p.Events)
}

func (p *Parser) SetDates(start, end string) {
	var s, e time.Time
	s = p.time.AddDate(0, 0, parseWeekday(start))
	if end != "" {
		e = p.time.AddDate(0, 0, parseWeekday(end))
		if s.After(e) {
			s, e = e, s
		}
	}
	p.dates = DateRange{Start: s, End: e}
}

func parseWeekday(weekday string) int {
	switch strings.ToLower(weekday) {
	case "sunday":
		return 3
	case "thursday":
		return 0
	case "friday":
		return 1
	case "saturday":
		return 2
	default:
		panic(weekday)
	}
}

func (p *Parser) Peek() html.TokenType {
	if p.tt != nil {
		return *p.tt
	}
	tt := p.next()
	p.tt = &tt
	return tt
}

func (p *Parser) Next() html.TokenType {
	if p.tt != nil {
		tt := *p.tt
		p.tt = nil
		return tt
	}
	return p.next()
}

// z.TagName() can only be called once, cache it
func (p *Parser) TagName() string {
	if p.tn != "" {
		return p.tn
	}
	tn, _ := p.tokenizer.TagName()
	p.tn = string(tn)
	return p.tn
}

func (p *Parser) next() html.TokenType {
	p.tn = ""
	return p.tokenizer.Next()
}

type ParserFunc func() ParserFunc

func (p *Parser) ParseRoot() ParserFunc {
	z := p.tokenizer
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.StartTagToken:
			tn := p.TagName()
			switch tn {
			case "h1", "h2", "h3", "h4", "h5":
				p.headerTag = tn
				return p.ParseHeader
			}
		}
	}
}

// ParseHeader consumes an <h*> tag, and forwards to ParseSection
func (p *Parser) ParseHeader() ParserFunc {
	var s string
	z := p.tokenizer
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.TextToken:
			s = string(z.Text())
		case html.EndTagToken:
			tn := p.TagName()
			switch tn {
			case "h1", "h2", "h3", "h4", "h5":
				match := eventPattern.FindAllStringSubmatch(s, 1)
				if match != nil {
					p.SetDates(match[0][1], match[0][4])
					return p.ParseSection
				}
				return p.ParseRoot
			}
		}
	}
}

// ParseSection consumes the tags from one <hX> to the next
func (p *Parser) ParseSection() ParserFunc {
	z := p.tokenizer
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.StartTagToken:
			tn := p.TagName()
			switch tn {
			// End of this section, beginning of next
			case "h1", "h2", "h3", "h4", "h5":
				p.headerTag = tn
				return p.ParseHeader
			case "p":
				p.event = &Event{Dates: p.dates}
				return p.ParseParagraph
			}
		}
	}
}

// ParseParagraph consumes a <p> tag, parsing <a> and <b> tags,
// formatting <li> tags, and ignoring <ul>, <span>, and <div>.
// All other tags signal a </p> and the Event is emitted.
func (p *Parser) ParseParagraph() ParserFunc {
	z := p.tokenizer
	for {
		tt := p.Peek()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.TextToken:
			p.event.Body.Write(z.Text())
		case html.StartTagToken:
			tn := p.TagName()
			switch tn {
			case "b":
				return p.ParseBold
			case "a":
				return p.ParseLink
			case "li":
				fmt.Fprintf(&p.event.Body, "- ")
			case "ul", "span", "div":
				// ignore
			default:
				p.Events <- p.event
				return p.ParseSection
			}
		case html.EndTagToken:
			tn := p.TagName()
			switch tn {
			case "p":
				p.Events <- p.event
				return p.ParseSection
			}
		}
		p.Next()
	}
}

// ParseLink consumes an <a> tag and writes its content to the
// event Body, followed by the parenthesized contents of the href attribute.
func (p *Parser) ParseLink() ParserFunc {
	z := p.tokenizer
	var href string
	var isLink bool
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.TextToken:
			txt := z.Text()
			if linkTextPattern.Match(txt) {
				isLink = true
			} else {
				p.event.Body.Write(txt)
			}
			p.event.Body.Write(z.Text())
		case html.StartTagToken:
			tn := p.TagName()
			switch tn {
			case "a":
				for {
					key, values, more := z.TagAttr()
					if !more {
						break
					}
					switch string(key) {
					case "href":
						href = string(values)
						break
					}
				}
			}
		case html.EndTagToken:
			tn := p.TagName()
			switch tn {
			case "a":
				if isLink {
					p.event.Link = href
				} else {
					fmt.Fprintf(&p.event.Body, " (%s)", href)
				}
				return p.ParseParagraph
			}
		}
	}
}

// ParseBold consumes a <b> tag and writes the contents either
// to an unpopulated event Title, or appends to its Body.
func (p *Parser) ParseBold() ParserFunc {
	z := p.tokenizer
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.TextToken:
			if p.event.Title == "" {
				p.event.Title = string(z.Text())
			} else {
				p.event.Body.Write(z.Text())
			}
		case html.EndTagToken:
			tn := p.TagName()
			// End of this section, beginning of next
			switch tn {
			case "b":
				return p.ParseParagraph
			}
		}
	}
}

// IgnoreTag consumes a <style> tag
func (p *Parser) IgnoreTag() ParserFunc {
	z := p.tokenizer
	for {
		tt := p.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				return nil
			}
			log.Fatal(z.Err())
		case html.EndTagToken:
			tn := p.TagName()
			switch tn {
			case "style", "script":
				return p.ParseParagraph
			}
		}
	}
}

func finalize(events chan *Event) chan *Event {
	out := make(chan *Event)
	go func() {
		for event := range events {
			event.Title = strings.Trim(event.Title, " 	/")
			if event.Title == "" {
				continue
			}
			match := locationTextPattern.FindAllStringSubmatch(event.Title, 1)
			if match != nil {
				event.Title = match[0][1]
				event.Location = match[0][4]
			}

			event.Description = strings.TrimSpace(event.Body.String())

			if event.Dates.End.IsZero() {
				date := event.Dates.Start
				var times TimeRange
				// Look for times or time ranges e.g. 7 p.m. or 11 a.m. - 6 p.m.
				match := singleDayTimePattern.FindAllStringSubmatch(event.Description, 1)
				if match != nil {
					st := match[0][2]  // 3 or 3:04
					sm := match[0][6]  // a or p or blank
					et := match[0][8]  // 3 or 3:04
					em := match[0][10] // a or p
					if sm == "" {
						sm = em
					}

					parts := strings.SplitN(st, ":", 2)
					if len(parts) == 1 {
						st = parts[0] + ":00"
					}
					sf := fmt.Sprintf("%s %sM", st, strings.ToUpper(sm))
					stime, err := time.Parse("3:04 PM", sf)
					if err != nil {
						log.Fatal(err)
					}
					times.Start = time.Date(date.Year(), date.Month(), date.Day(), stime.Hour(), stime.Minute(), stime.Second(), stime.Nanosecond(), date.Location())
					if et == "" {
						times.End = times.Start.Add(1 * time.Hour)
					} else {
						parts = strings.SplitN(et, ":", 2)
						if len(parts) == 1 {
							et = parts[0] + ":00"
						}
						ef := fmt.Sprintf("%s %sM", et, strings.ToUpper(em))
						etime, err := time.Parse("3:04 PM", ef)
						if err != nil {
							log.Fatal(err)
						}
						times.End = time.Date(date.Year(), date.Month(), date.Day(), etime.Hour(), etime.Minute(), etime.Second(), etime.Nanosecond(), date.Location())
					}
					event.Times = append(event.Times, times)
				}
			} else {
				// Include the end date
				event.Dates.End = event.Dates.End.AddDate(0, 0, 1)
			}
			out <- event
		}
		close(out)
	}()
	return out
}

type Server struct {
	templates *template.Template
	mux       *http.ServeMux
	client    http.Client
	cache     *expirable.LRU[string, []*Event]
}

func NewServer(templates *template.Template) http.Handler {
	s := &Server{
		templates: templates,
		cache:     expirable.NewLRU[string, []*Event](10, nil, 15*time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /calbot/soiree/", s.Generate)
	s.mux = mux

	mux = http.NewServeMux()
	mux.Handle("GET /calbot/", s)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
		fmt.Fprint(w, "ok")
	})
	return mux
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) Generate(w http.ResponseWriter, req *http.Request) {
	// The Weekend Guide is published each Thursday, find most recent Thursday
	t := time.Now()
	weekday := t.Weekday()
	if weekday >= time.Thursday {
		t = t.AddDate(0, 0, -int(weekday-time.Thursday))
	} else {
		t = t.AddDate(0, 0, int(time.Thursday-weekday)-7)
	}

	u := fmt.Sprintf("https://www.littlerocksoiree.com/little-rock-weekend-guide-%s-%d-%d/", strings.ToLower(t.Format("Jan")), t.Day(), t.Day()+3)
	events, ok := s.cache.Get(u)
	if !ok {
		req, err := http.NewRequestWithContext(req.Context(), http.MethodGet, u, nil)
		if err != nil {
			log.Fatal(err)
		}
		req.Header.Add("User-Agent", "CalBot/1.0")
		resp, err := s.client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()

		p := NewParser(t, resp.Body)
		go p.Run()
		for event := range finalize(p.Events) {
			events = append(events, event)
		}
		s.cache.Add(u, events)
	}

	w.Header().Add("Content-Type", "text/calendar")
	err := s.templates.ExecuteTemplate(w, "feed.ics.tmpl", events)
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	flag.Parse()

	templates, err := template.New("templates").Funcs(map[string]any{
		"time": func(t time.Time) string {
			return t.Format("20060102T150405")
		},
		"date": func(t time.Time) string {
			return t.Format("20060102")
		},
		"now": func() time.Time {
			return time.Now()
		},
		"uuid": func() string {
			return uuid.NewString()
		},
		"escape": func(text string) string {
			return strings.ReplaceAll(text, "\n", "\\n")
		},
		"wrap": func(text string) string {
			width := 58
			prefix := len("DESCRIPTION:")
			pad := ""
			nl := "\n"
			var out string
			for i := 0; len(text) > 0; i++ {
				l := width - prefix
				prefix = 0
				if len(text) < l {
					l = len(text)
					nl = ""
				}
				out += pad + text[:l] + nl
				pad = " "
				text = text[l:]
			}
			return out
		},
	}).ParseGlob(path.Join(*templatesPath, "*.tmpl"))
	if err != nil {
		log.Fatal(err)
	}

	server := http.Server{Addr: *address, Handler: logging.NewLoggingHandler(NewServer(templates), logging.NewApacheLogger(logger))}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Println("starting server")
	go func() {
		err = server.ListenAndServe()
		switch err {
		case nil, http.ErrServerClosed:
		default:
			log.Fatal("listen and serve", err)
		}
	}()

	<-done
	log.Println("stopping server")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		log.Fatalf("shutdown server: %v", err)
	}
	log.Println("server stopped")
}
