package main

import (
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// coreIPCHandler is the app-facing side of the IPC bridge.
//
// It receives commands and cookies from the mobile app and forwards them
// to the manager. When the core needs fresh cookies (SmartCaptcha), the
// manager calls SetCaptchaNotifier, whose callback fires into IPC.
type coreIPCHandler struct {
	manager *manager.Manager
	// exchanger takes the cookies on the classic path without a Session
	// (manager nil). Nil when that carrier keeps no cookies.
	exchanger transport.CookieExchanger
}

func (h *coreIPCHandler) OnConnect()    { utils.Debugf("[IPC] app connected") }
func (h *coreIPCHandler) OnDisconnect() { utils.Debugf("[IPC] app disconnected") }

func (h *coreIPCHandler) OnCommand(p *ipc.CommandPayload) {
	utils.Debugf("[IPC] command action=%q params=%v", p.Action, p.Params)
	// No dynamic commands yet. Reserved for start/stop/restart from the UI.
}

func (h *coreIPCHandler) OnCookies(p *ipc.CookiesOfferPayload) {
	if p == nil || p.Transport == "" || len(p.Jar) == 0 {
		utils.Debugf("[IPC] empty cookies offer, ignoring")
		return
	}
	if h.manager == nil {
		if h.exchanger == nil || p.Remote {
			utils.Debugf("[IPC] cookies for %q: nothing on this path takes them", p.Transport)
			return
		}
		if err := h.exchanger.ApplyCookies(p.Jar); err != nil {
			utils.Debugf("[IPC] apply cookies for %q: %v", p.Transport, err)
			return
		}
		utils.Debugf("[IPC] applied %d cookies for %q", len(p.Jar), p.Transport)
		return
	}
	if p.Remote {
		if err := h.manager.OfferCookies(p.Transport, p.Jar); err != nil {
			utils.Debugf("[IPC] offer cookies to exit for %q: %v", p.Transport, err)
			return
		}
		utils.Debugf("[IPC] sent %d cookies to exit for %q", len(p.Jar), p.Transport)
		return
	}
	if err := h.manager.AcceptCookies(p.Transport, p.Jar); err != nil {
		utils.Debugf("[IPC] apply cookies for %q: %v", p.Transport, err)
		return
	}
	utils.Debugf("[IPC] applied %d cookies for %q", len(p.Jar), p.Transport)
}
