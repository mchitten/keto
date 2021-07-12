// (c) Copyright IBM Corp. 2021
// (c) Copyright Instana Inc. 2016

package instana

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	f "github.com/looplab/fsm"
)

const (
	eInit     = "init"
	eLookup   = "lookup"
	eAnnounce = "announce"
	eTest     = "test"

	retryPeriod    = 30 * 1000 * time.Millisecond
	maximumRetries = 2
)

type fsmS struct {
	agent   *agentS
	fsm     *f.FSM
	timer   *time.Timer
	retries int
}

var procSchedPIDRegex = regexp.MustCompile(`\((\d+),`)

func newFSM(agent *agentS) *fsmS {
	agent.logger.Warn("Stan is on the scene. Starting Instana instrumentation.")
	agent.logger.Debug("initializing fsm")

	ret := &fsmS{
		agent:   agent,
		retries: maximumRetries,
	}

	ret.fsm = f.NewFSM(
		"none",
		f.Events{
			{Name: eInit, Src: []string{"none", "unannounced", "announced", "ready"}, Dst: "init"},
			{Name: eLookup, Src: []string{"init"}, Dst: "unannounced"},
			{Name: eAnnounce, Src: []string{"unannounced"}, Dst: "announced"},
			{Name: eTest, Src: []string{"announced"}, Dst: "ready"}},
		f.Callbacks{
			"init":              ret.lookupAgentHost,
			"enter_unannounced": ret.announceSensor,
			"enter_announced":   ret.testAgent,
		})
	ret.fsm.Event(eInit)

	return ret
}

func (r *fsmS) scheduleRetry(e *f.Event, cb func(e *f.Event)) {
	r.timer = time.NewTimer(retryPeriod)
	go func() {
		<-r.timer.C
		cb(e)
	}()
}

func (r *fsmS) lookupAgentHost(e *f.Event) {
	cb := func(found bool, host string) {
		if found {
			r.lookupSuccess(host)
			return
		}

		gateway, err := getDefaultGateway("/proc/net/route")
		if err != nil {
			r.agent.logger.Error("failed to fetch the default gateway, scheduling retry: ", err)
			r.scheduleRetry(e, r.lookupAgentHost)

			return
		}

		if gateway == "" {
			r.agent.logger.Error("default gateway not available, scheduling retry")
			r.scheduleRetry(e, r.lookupAgentHost)

			return
		}

		go r.checkHost(gateway, func(found bool, host string) {
			if found {
				r.lookupSuccess(host)
				return
			}

			r.agent.logger.Error("cannot connect to the agent through localhost or default gateway, scheduling retry")
			r.scheduleRetry(e, r.lookupAgentHost)
		})

	}

	go r.checkHost(r.agent.host, cb)
}

func (r *fsmS) checkHost(host string, cb func(found bool, host string)) {
	r.agent.logger.Debug("checking host ", host)

	header, err := r.agent.requestHeader(r.agent.makeHostURL(host, "/"), "GET", "Server")

	cb(err == nil && header == agentHeader, host)
}

func (r *fsmS) lookupSuccess(host string) {
	r.agent.logger.Debug("agent lookup success ", host)

	r.agent.setHost(host)
	r.retries = maximumRetries
	r.fsm.Event(eLookup)
}

func (r *fsmS) announceSensor(e *f.Event) {
	cb := func(success bool, resp agentResponse) {
		if !success {
			r.agent.logger.Error("Cannot announce sensor. Scheduling retry.")
			r.retries--
			if r.retries == 0 {
				r.fsm.Event(eInit)
				return
			}

			r.scheduleRetry(e, r.announceSensor)

			return
		}

		r.agent.logger.Info("Host agent available. We're in business. Announced pid:", resp.Pid)
		r.agent.applyHostAgentSettings(resp)

		r.retries = maximumRetries
		r.fsm.Event(eAnnounce)
	}

	r.agent.logger.Debug("announcing sensor to the agent")

	go func(cb func(success bool, resp agentResponse)) {
		defer func() {
			if err := recover(); err != nil {
				r.agent.logger.Debug("Announce recovered:", err)
			}
		}()

		pid := 0
		schedFile := fmt.Sprintf("/proc/%d/sched", os.Getpid())
		if _, err := os.Stat(schedFile); err == nil {
			sf, err := os.Open(schedFile)
			defer sf.Close() //nolint:staticcheck

			if err == nil {
				fscanner := bufio.NewScanner(sf)
				fscanner.Scan()
				primaLinea := fscanner.Text()

				match := procSchedPIDRegex.FindStringSubmatch(primaLinea)
				i, err := strconv.Atoi(match[1])
				if err == nil {
					pid = i
				}
			}
		}

		if pid == 0 {
			pid = os.Getpid()
		}

		d := &discoveryS{
			PID:  pid,
			Name: os.Args[0],
			Args: os.Args[1:],
		}
		if name, args, ok := getProcCommandLine(); ok {
			r.agent.logger.Debug("got cmdline from /proc: ", name, args)
			d.Name, d.Args = name, args
		} else {
			r.agent.logger.Debug("no /proc, using OS reported cmdline")
		}

		if _, err := os.Stat("/proc"); err == nil {
			if addr, err := net.ResolveTCPAddr("tcp", r.agent.host+":42699"); err == nil {
				if tcpConn, err := net.DialTCP("tcp", nil, addr); err == nil {
					defer tcpConn.Close()

					f, err := tcpConn.File()

					if err != nil {
						r.agent.logger.Error(err)
					} else {
						d.Fd = fmt.Sprintf("%v", f.Fd())

						link := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), f.Fd())
						if _, err := os.Stat(link); err == nil {
							d.Inode, _ = os.Readlink(link)
						}
					}
				}
			}
		}

		var resp agentResponse
		_, err := r.agent.requestResponse(r.agent.makeURL(agentDiscoveryURL), "PUT", d, &resp)
		cb(err == nil, resp)
	}(cb)
}

func (r *fsmS) testAgent(e *f.Event) {
	cb := func(b bool) {
		if b {
			r.retries = maximumRetries
			r.fsm.Event(eTest)
		} else {
			r.agent.logger.Debug("Agent is not yet ready. Scheduling retry.")
			r.retries--
			if r.retries > 0 {
				r.scheduleRetry(e, r.testAgent)
			} else {
				r.fsm.Event(eInit)
			}
		}
	}

	r.agent.logger.Debug("testing communication with the agent")

	go func(cb func(b bool)) {
		_, err := r.agent.head(r.agent.makeURL(agentDataURL))
		cb(err == nil)
	}(cb)
}

func (r *fsmS) reset() {
	r.retries = maximumRetries
	r.fsm.Event(eInit)
}
