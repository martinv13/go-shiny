package appproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/martinv13/go-shiny/models"
	"github.com/martinv13/go-shiny/modules/config"
	"github.com/martinv13/go-shiny/modules/ssehandler"
	uuid "github.com/satori/go.uuid"
)

type Session struct {
	ID         string
	startedAt  int64
	lastActive int64
	App        *AppProxy
	Instance   *appInstance
}

// Close a session and remove it from parent app object
func (sess *Session) Close() {
	app := sess.App
	app.Lock()
	delete(app.Sessions, sess.ID)
	app.Unlock()
	app.reportStatus()
}

type AppProxy struct {
	sync.RWMutex
	ShinyApp     models.ShinyApp
	StatusStream *ssehandler.MessageBroker
	Instances    map[int]*appInstance
	nextID       int
	Sessions     map[string]*Session
	config       *config.Config
}

// Create a new app proxy
func NewAppProxy(app models.ShinyApp, msgBroker *ssehandler.MessageBroker, config *config.Config) (*AppProxy, error) {

	appProxy := &AppProxy{
		ShinyApp:     app,
		StatusStream: msgBroker,
		nextID:       0,
		Sessions:     map[string]*Session{},
		Instances:    map[int]*appInstance{},
		config:       config,
	}

	appProxy.Lock()
	defer appProxy.Unlock()

	for w := 0; w < app.Workers; w++ {
		inst := &appInstance{App: app, config: config}
		inst.Start()
		appProxy.Instances[appProxy.nextID] = inst
		appProxy.nextID++
	}
	return appProxy, nil
}

// Stop or restart all instances, while keeping existing connections
func (p *AppProxy) phaseOut(restart bool) {
	for _, i := range p.Instances {
		i.PhaseOut()
	}
	if restart {
		for w := 0; w < p.ShinyApp.Workers; w++ {
			inst := &appInstance{App: p.ShinyApp, config: p.config}
			inst.Start()
			p.Instances[p.nextID] = inst
			p.nextID++
		}
	}
}

// Apply changes to app settings
func (p *AppProxy) Update(app models.ShinyApp) {
	p.Lock()
	defer p.Unlock()

	prevApp := p.ShinyApp
	p.ShinyApp = app

	if !app.Active {
		p.phaseOut(false)
	} else {
		if prevApp.AppDir != app.AppDir || !prevApp.Active {
			p.phaseOut(true)
		}
	}
}

// Return app status info as a map
func (app *AppProxy) GetStatus(detailed bool) map[string]interface{} {

	nbRunning := 0
	nbRollingout := 0
	stdErr := []string{}
	for _, i := range app.Instances {
		if i.Status == "RUNNING" {
			nbRunning++
		} else if i.Status == "ROLLING_OUT" {
			nbRollingout++
		}
		if detailed {
			stdErr = append(stdErr, i.StdErr)
		}
	}
	status := map[string]interface{}{
		"RunningInst":    nbRunning,
		"RollingOutInst": nbRollingout,
		"ConnectedUsers": len(app.Sessions),
	}
	if detailed {
		status["StdErr"] = stdErr
	}
	return status
}

// Stream status update
func (appProxy *AppProxy) reportStatus() {
	appProxy.RLock()
	defer appProxy.RUnlock()
	users := len(appProxy.Sessions)
	msg := ""
	if users == 0 {
		msg = "no connected user"
	} else if users == 1 {
		msg = "1 connected user"
	} else {
		msg = fmt.Sprintf("%d connected users", users)
	}
	msgData, _ := json.Marshal(map[string]string{
		"appName": appProxy.ShinyApp.AppName,
		"value":   msg,
	})
	appProxy.StatusStream.Message <- string(msgData)
}

// GetSession returns an existing session or a new session and selects
// the most appropriate running instance according to current load
func (appProxy *AppProxy) GetSession(sessionID string) (*Session, error) {
	appProxy.Lock()
	defer func() {
		appProxy.Unlock()
		appProxy.reportStatus()
	}()

	session, ok := appProxy.Sessions[sessionID]

	if ok {
		if session.Instance.Status == "RUNNING" {
			session.lastActive = time.Now().Unix()
			return session, nil
		}
		if len(appProxy.Instances) > 0 {
			for _, inst := range appProxy.Instances {
				if inst.Status == "RUNNING" {
					session.Instance = inst
					session.lastActive = time.Now().Unix()
					return session, nil
				}
			}
		}
	}

	if len(appProxy.Instances) > 0 {
		for _, inst := range appProxy.Instances {
			if inst.Status == "RUNNING" {
				now := time.Now().Unix()
				session = &Session{
					ID:         uuid.NewV4().String(),
					startedAt:  now,
					lastActive: now,
					App:        appProxy,
					Instance:   inst,
				}
				appProxy.Sessions[session.ID] = session
				return session, nil
			}
		}
	}
	return nil, errors.New("No running instance available")
}
