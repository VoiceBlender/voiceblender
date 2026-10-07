package api

import (
	"context"
	"net/http"
	"time"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CreateTrunkResponse is the body returned by POST /v1/sip/trunks.
type CreateTrunkResponse struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// TrunksListResponse is the body returned by GET /v1/sip/trunks.
type TrunksListResponse struct {
	Trunks []sipmod.TrunkView `json:"trunks"`
}

// doCreateTrunk validates the request, registers the trunk, and starts its
// background lifecycle. Shared by the REST handler and the VSI dispatcher.
func (s *Server) doCreateTrunk(req CreateTrunkRequest) (CreateTrunkResponse, error) {
	switch req.Type {
	case string(sipmod.TrunkTypeSIPRegister):
		return s.doCreateSIPRegisterTrunk(req)
	case string(sipmod.TrunkTypeIPIP):
		return s.doCreateIPIPTrunk(req)
	case "":
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "type is required")
	default:
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "unknown trunk type: %s", req.Type)
	}
}

func (s *Server) doCreateSIPRegisterTrunk(req CreateTrunkRequest) (CreateTrunkResponse, error) {
	spec := req.SIPRegister
	if spec == nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register block is required when type=sip_register")
	}
	if spec.RegistrarURI == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.registrar_uri is required")
	}
	if spec.AOR == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.aor is required")
	}
	if spec.Password == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.password is required")
	}

	var registrarURI sip.Uri
	if err := sip.ParseUri(spec.RegistrarURI, &registrarURI); err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.registrar_uri is invalid: %s", err.Error())
	}
	var aorURI sip.Uri
	if err := sip.ParseUri(spec.AOR, &aorURI); err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.aor is invalid: %s", err.Error())
	}

	outboundProxy, err := s.resolveTrunkProxy("sip_register.outbound_proxy", spec.OutboundProxy)
	if err != nil {
		return CreateTrunkResponse{}, err
	}
	// A TLS proxy without a TLS listener still registers, but the Contact can
	// only advertise the UDP socket — so the upstream sends calls back in the
	// clear. Nothing downstream surfaces that, hence the warning here.
	if outboundProxy != nil && sipmod.TransportForURI(*outboundProxy) == "tls" &&
		s.SIPEngine != nil && s.SIPEngine.TLSPort() == 0 {
		s.Log.Warn("outbound proxy uses TLS but SIP_TLS_PORT is not configured; "+
			"REGISTER Contact will advertise the plaintext UDP socket",
			"proxy", outboundProxy.String())
	}

	id := uuid.NewString()
	cfg := sipmod.OutboundRegistrationConfig{
		DefaultExpiresSeconds: s.Config.SIPOutboundRegistrationDefaultExpiresSeconds,
		MinExpiresSeconds:     s.Config.SIPOutboundRegistrationMinExpiresSeconds,
		MaxExpiresSeconds:     s.Config.SIPOutboundRegistrationMaxExpiresSeconds,
		RefreshRatio:          s.Config.SIPOutboundRegistrationRefreshRatio,
		FailureBackoffMax:     time.Duration(s.Config.SIPOutboundRegistrationFailureBackoffMaxMs) * time.Millisecond,
	}
	trunk := sipmod.NewOutboundRegistration(s.SIPEngine, s.Bus, s.Log, cfg, sipmod.OutboundRegistrationParams{
		ID:                      id,
		AppID:                   req.AppID,
		RegistrarURI:            registrarURI,
		AOR:                     aorURI,
		Username:                spec.Username,
		Password:                spec.Password,
		ContactUser:             spec.ContactUser,
		RequestedExpiresSeconds: spec.ExpiresSeconds,
		OutboundProxy:           outboundProxy,
		TLSInsecureSkipVerify:   spec.TLSInsecureSkipVerify,
	})
	s.SIPEngine.Trunks().Add(trunk)
	// The trunk lifecycle outlives the request that created it — using a
	// request-scoped context would cancel the REGISTER loop immediately.
	trunk.Start(context.Background())

	return CreateTrunkResponse{
		ID:     id,
		Type:   string(sipmod.TrunkTypeSIPRegister),
		Status: string(sipmod.TrunkStatusRegistering),
	}, nil
}

// resolveTrunkProxy returns the trunk's outbound proxy: the per-trunk value, or
// the global default. Resolved at create time rather than per request, so the
// trunk snapshot reports the next hop actually in effect.
func (s *Server) resolveTrunkProxy(field, raw string) (*sip.Uri, error) {
	if raw != "" {
		u, err := sipmod.ParseProxyURI(raw)
		if err != nil {
			return nil, newAPIError(http.StatusBadRequest, "%s is invalid: %s", field, err.Error())
		}
		return &u, nil
	}
	if s.Config.SIPOutboundProxy == "" {
		return nil, nil
	}
	u, err := sipmod.ParseProxyURI(s.Config.SIPOutboundProxy)
	if err != nil {
		s.Log.Warn("SIP_OUTBOUND_PROXY is invalid; trunk will route at its upstream", "error", err)
		return nil, nil
	}
	return &u, nil
}

const (
	maxIPIPInboundSources   = 64
	maxIPIPPingIntervalSecs = 3600
)

func (s *Server) doCreateIPIPTrunk(req CreateTrunkRequest) (CreateTrunkResponse, error) {
	spec := req.IPIP
	if spec == nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip block is required when type=ip_ip")
	}
	if spec.PeerURI == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.peer_uri is required")
	}
	peerURI, err := sipmod.ParseProxyURI(spec.PeerURI)
	if err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.peer_uri is invalid: %s", err.Error())
	}

	var aor *sip.Uri
	if spec.AOR != "" {
		var u sip.Uri
		if err := sip.ParseUri(spec.AOR, &u); err != nil {
			return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.aor is invalid: %s", err.Error())
		}
		if u.User == "" || u.Host == "" {
			return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.aor is invalid: must have a user and a host")
		}
		aor = &u
	}

	if spec.Username != "" && spec.Password == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.password is required when ip_ip.username is set")
	}
	if spec.Password != "" && spec.Username == "" && aor == nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.username is required when ip_ip.password is set without ip_ip.aor")
	}

	if len(spec.InboundSources) > maxIPIPInboundSources {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.inbound_sources has too many entries (max %d)", maxIPIPInboundSources)
	}
	sources, err := sipmod.ParseSourcePrefixes(spec.InboundSources)
	if err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.inbound_sources is invalid: %s", err.Error())
	}
	for _, p := range sources {
		if p.Bits() == 0 {
			return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.inbound_sources is invalid: %s matches every address", p.String())
		}
	}

	if spec.OptionsPingIntervalSeconds < 0 || spec.OptionsPingIntervalSeconds > maxIPIPPingIntervalSecs {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "ip_ip.options_ping_interval_seconds must be between 0 and %d", maxIPIPPingIntervalSecs)
	}

	outboundProxy, err := s.resolveTrunkProxy("ip_ip.outbound_proxy", spec.OutboundProxy)
	if err != nil {
		return CreateTrunkResponse{}, err
	}

	id := uuid.NewString()
	trunk := sipmod.NewIPIPTrunk(s.SIPEngine, s.Bus, s.Log, sipmod.IPIPTrunkParams{
		ID:                    id,
		AppID:                 req.AppID,
		PeerURI:               peerURI,
		OutboundProxy:         outboundProxy,
		AOR:                   aor,
		Username:              spec.Username,
		Password:              spec.Password,
		InboundSources:        sources,
		PingInterval:          time.Duration(spec.OptionsPingIntervalSeconds) * time.Second,
		TLSInsecureSkipVerify: spec.TLSInsecureSkipVerify,
	})
	s.SIPEngine.Trunks().Add(trunk)
	trunk.Start(context.Background())

	return CreateTrunkResponse{
		ID:     id,
		Type:   string(sipmod.TrunkTypeIPIP),
		Status: string(sipmod.TrunkStatusActive),
	}, nil
}

// doListTrunks returns a snapshot of every configured trunk.
func (s *Server) doListTrunks() TrunksListResponse {
	trunks := s.SIPEngine.Trunks().List()
	views := make([]sipmod.TrunkView, 0, len(trunks))
	for _, t := range trunks {
		views = append(views, t.Snapshot())
	}
	return TrunksListResponse{Trunks: views}
}

// doGetTrunk returns a single trunk snapshot or a 404 apiError.
func (s *Server) doGetTrunk(id string) (sipmod.TrunkView, error) {
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		return sipmod.TrunkView{}, newAPIError(http.StatusNotFound, "trunk not found")
	}
	return t.Snapshot(), nil
}

// doDeleteTrunk stops and removes the trunk asynchronously.
func (s *Server) doDeleteTrunk(id string) error {
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		return newAPIError(http.StatusNotFound, "trunk not found")
	}
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.Stop(stopCtx)
		s.SIPEngine.Trunks().Remove(id)
	}()
	return nil
}

// createTrunk handles POST /v1/sip/trunks. Returns 202 Accepted.
func (s *Server) createTrunk(w http.ResponseWriter, r *http.Request) {
	var req CreateTrunkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doCreateTrunk(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// listTrunks handles GET /v1/sip/trunks.
func (s *Server) listTrunks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.doListTrunks())
}

// getTrunk handles GET /v1/sip/trunks/{id}.
func (s *Server) getTrunk(w http.ResponseWriter, r *http.Request) {
	view, err := s.doGetTrunk(chi.URLParam(r, "id"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// deleteTrunk handles DELETE /v1/sip/trunks/{id}. Returns 202 Accepted and
// performs the teardown asynchronously.
func (s *Server) deleteTrunk(w http.ResponseWriter, r *http.Request) {
	if err := s.doDeleteTrunk(chi.URLParam(r, "id")); err != nil {
		handleAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
