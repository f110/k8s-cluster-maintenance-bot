package webhook

import (
	"io/ioutil"
	"log"
	"net/http"

	"github.com/google/go-github/v29/github"
)

const (
	EventTypePush        = "push"
	EventTypePullRequest = "pull_request"
)

type subscriber struct {
	Owner       string
	Repo        string
	ConsumeFunc ConsumeFunc
}

type ConsumeFunc func(event interface{})

type eventHandler struct {
	subscribers map[string][]*subscriber
}

func newEventHandler() *eventHandler {
	return &eventHandler{
		subscribers: make(map[string][]*subscriber),
	}
}

func (e *eventHandler) SubscribePushEvent(consume ConsumeFunc) {
	if _, ok := e.subscribers[EventTypePush]; !ok {
		e.subscribers[EventTypePush] = make([]*subscriber, 0)
	}

	e.subscribers[EventTypePush] = append(e.subscribers[EventTypePush], &subscriber{ConsumeFunc: consume})
}

func (e *eventHandler) SubscribePullRequest(consume ConsumeFunc) {
	if _, ok := e.subscribers[EventTypePullRequest]; !ok {
		e.subscribers[EventTypePullRequest] = make([]*subscriber, 0)
	}

	e.subscribers[EventTypePullRequest] = append(e.subscribers[EventTypePullRequest], &subscriber{ConsumeFunc: consume})
}

func (e *eventHandler) Handle(msg interface{}) {
	switch event := msg.(type) {
	case *github.PushEvent:
		subscribers, ok := e.subscribers[EventTypePush]
		if !ok {
			return
		}

		log.Printf("Push Event: %s", event.GetRepo().GetFullName())
		for _, s := range subscribers {
			log.Print("Trigger subscriber")
			go s.ConsumeFunc(event)
		}
	case *github.PullRequestEvent:
		subscribers, ok := e.subscribers[EventTypePullRequest]
		if !ok {
			return
		}

		log.Printf("PullRequest: %s", event.GetRepo().GetFullName())
		for _, s := range subscribers {
			log.Print("Trigger subscriber")
			go s.ConsumeFunc(event)
		}
	}
}

type WebhookListener struct {
	*http.Server
	*eventHandler
}

func NewWebhookListener(addr string) *WebhookListener {
	l := &WebhookListener{}

	m := http.NewServeMux()
	m.HandleFunc("/github", func(w http.ResponseWriter, req *http.Request) {
		wType := github.WebHookType(req)
		buf, err := ioutil.ReadAll(req.Body)
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req.Body.Close()

		messageBody, err := github.ParseWebHook(wType, buf)
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		log.Printf("Get event: %s %v", wType, messageBody)
		l.Handle(messageBody)
	})

	s := &http.Server{
		Addr:    addr,
		Handler: m,
	}
	l.Server = s
	l.eventHandler = newEventHandler()

	return l
}
