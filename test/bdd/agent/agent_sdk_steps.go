/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package agent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/google/uuid"
	jsonld "github.com/piprate/json-gold/ld"

	"github.com/hyperledger/aries-framework-go/component/storage/leveldb"
	"github.com/hyperledger/aries-framework-go/component/storageutil/cachedstore"
	"github.com/hyperledger/aries-framework-go/component/storageutil/mem"
	"github.com/hyperledger/aries-framework-go/pkg/common/log"
	remotecrypto "github.com/hyperledger/aries-framework-go/pkg/crypto/webkms"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/messaging/msghandler"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/decorator"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/transport"
	arieshttp "github.com/hyperledger/aries-framework-go/pkg/didcomm/transport/http"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/transport/ws"
	"github.com/hyperledger/aries-framework-go/pkg/doc/ld"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries/defaults"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/webkms"
	ldstore "github.com/hyperledger/aries-framework-go/pkg/store/ld"
	"github.com/hyperledger/aries-framework-go/pkg/vdr/httpbinding"
	"github.com/hyperledger/aries-framework-go/spi/storage"
	"github.com/hyperledger/aries-framework-go/test/bdd/pkg/context"
	didexchangebdd "github.com/hyperledger/aries-framework-go/test/bdd/pkg/didexchange"
	bddldcontext "github.com/hyperledger/aries-framework-go/test/bdd/pkg/ldcontext"
)

const (
	dbPath = "./db"

	httpTransportProvider      = "http"
	webSocketTransportProvider = "websocket"
	sideTreeURL                = "${SIDETREE_URL}"
)

var logger = log.New("aries-framework/tests")

// SDKSteps contains steps for agent from client SDK.
type SDKSteps struct {
	bddContext           *context.BDDContext
	didExchangeSDKS      *didexchangebdd.SDKSteps
	newKeyType           kms.KeyType
	newKeyAgreementType  kms.KeyType
	newMediaTypeProfiles []string
}

// NewSDKSteps returns new agent from client SDK.
func NewSDKSteps() *SDKSteps {
	return &SDKSteps{}
}

func (a *SDKSteps) scenario(keyType, keyAgreementType, mediaTypeProfile string) error {
	a.newKeyType = kms.KeyType(keyType)
	a.newKeyAgreementType = kms.KeyType(keyAgreementType)
	a.newMediaTypeProfiles = []string{mediaTypeProfile}

	return nil
}

func (a *SDKSteps) useMediaTypeProfiles(mediaTypeProfiles string) error {
	a.newMediaTypeProfiles = strings.Split(mediaTypeProfiles, ",")

	return nil
}

// CreateAgent with the given parameters.
func (a *SDKSteps) CreateAgent(agentID, inboundHost, inboundPort, scheme string) error {
	return a.createAgentByDIDCommVer(agentID, inboundHost, inboundPort, scheme, false)
}

// createAgentByDIDCommV2 with the given parameters.
func (a *SDKSteps) createAgentByDIDCommV2(agentID, inboundHost, inboundPort, scheme string) error {
	return a.createAgentByDIDCommVer(agentID, inboundHost, inboundPort, scheme, true)
}

func (a *SDKSteps) createConnectionV2(agent1, agent2 string) error {
	err := a.createAgentByDIDCommVer(agent1, "localhost", "random", "http", true)
	if err != nil {
		return fmt.Errorf("create agent %q: %w", agent1, err)
	}

	err = a.createAgentByDIDCommVer(agent2, "localhost", "random", "http", true)
	if err != nil {
		return fmt.Errorf("create agent %q: %w", agent2, err)
	}

	err = a.didExchangeSDKS.CreateDIDExchangeClient(strings.Join([]string{agent1, agent2}, ","))
	if err != nil {
		return err
	}

	err = a.didExchangeSDKS.RegisterPostMsgEvent(strings.Join([]string{agent1, agent2}, ","), "completed")
	if err != nil {
		return fmt.Errorf("failed to register agents for didexchange post msg events : %w", err)
	}

	err = a.didExchangeSDKS.CreateInvitation(agent1, "")
	if err != nil {
		return fmt.Errorf("create invitation: %w", err)
	}

	if err := a.didExchangeSDKS.ReceiveInvitation(agent2, agent1); err != nil {
		return fmt.Errorf("eeceive invitation: %w", err)
	}

	if err := a.didExchangeSDKS.ApproveRequest(agent2); err != nil {
		return fmt.Errorf("approve request %q: %w", agent2, err)
	}

	if err := a.didExchangeSDKS.ApproveRequest(agent1); err != nil {
		return fmt.Errorf("approve request %q: %w", agent1, err)
	}

	return a.didExchangeSDKS.WaitForPostEvent(strings.Join([]string{agent1, agent2}, ","), "completed")
}

// createAgentByDIDCommVer with the given parameters.
func (a *SDKSteps) createAgentByDIDCommVer(agentID, inboundHost, inboundPort, scheme string, useDIDCommV2 bool) error {
	storeProv := a.getStoreProvider(agentID)

	loader, err := createJSONLDDocumentLoader(storeProv)
	if err != nil {
		return fmt.Errorf("create document loader: %w", err)
	}

	opts := append([]aries.Option{}, aries.WithStoreProvider(storeProv), aries.WithJSONLDDocumentLoader(loader))

	if useDIDCommV2 {
		opts = append(opts, aries.WithMediaTypeProfiles([]string{transport.MediaTypeDIDCommV2Profile}))
	}

	return a.create(agentID, inboundHost, inboundPort, scheme, opts...)
}

// CreateAgentWithRemoteKMS with the given parameters with a remote kms.
func (a *SDKSteps) CreateAgentWithRemoteKMS(agentID, inboundHost, inboundPort, scheme, ksURL, controller string) error {
	storeProv := a.getStoreProvider(agentID)

	loader, err := createJSONLDDocumentLoader(storeProv)
	if err != nil {
		return fmt.Errorf("create document loader: %w", err)
	}

	opts := append([]aries.Option{}, aries.WithStoreProvider(storeProv), aries.WithJSONLDDocumentLoader(loader))

	cp, err := loadCertPool()
	if err != nil {
		return err
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: cp}, //nolint:gosec
		},
	}

	keyStoreURL, _, err := webkms.CreateKeyStore(httpClient, ksURL, controller, "")
	if err != nil {
		return fmt.Errorf("error calling CreateKeystore: %w", err)
	}

	rKMS := webkms.New(keyStoreURL, httpClient)

	opts = append(opts, aries.WithKMS(func(provider kms.Provider) (kms.KeyManager, error) {
		return rKMS, nil
	}))

	rCrypto := remotecrypto.New(keyStoreURL, httpClient)

	opts = append(opts, aries.WithCrypto(rCrypto))

	return a.create(agentID, inboundHost, inboundPort, scheme, opts...)
}

func loadCertPool() (*x509.CertPool, error) {
	cp := x509.NewCertPool()
	certPrefix := "fixtures/keys/tls/"

	pemPath := fmt.Sprintf("%sec-pubCert.pem", certPrefix)

	pemData, err := ioutil.ReadFile(pemPath) //nolint:gosec
	if err != nil {
		return nil, err
	}

	ok := cp.AppendCertsFromPEM(pemData)
	if !ok {
		return nil, errors.New("failed to append certs from PEM")
	}

	return cp, nil
}

func (a *SDKSteps) createAgentWithRegistrar(agentID, inboundHost, inboundPort, scheme string) error {
	msgRegistrar := msghandler.NewRegistrar()
	a.bddContext.MessageRegistrar[agentID] = msgRegistrar

	storeProv := a.getStoreProvider(agentID)

	loader, err := createJSONLDDocumentLoader(storeProv)
	if err != nil {
		return fmt.Errorf("create document loader: %w", err)
	}

	opts := append([]aries.Option{}, aries.WithStoreProvider(storeProv),
		aries.WithMessageServiceProvider(msgRegistrar), aries.WithJSONLDDocumentLoader(loader))

	return a.create(agentID, inboundHost, inboundPort, scheme, opts...)
}

func (a *SDKSteps) createAgentWithRegistrarAndHTTPDIDResolver(agentID, inboundHost, inboundPort,
	scheme, endpointURL, acceptDidMethod string) error {
	msgRegistrar := msghandler.NewRegistrar()
	a.bddContext.MessageRegistrar[agentID] = msgRegistrar

	url := a.bddContext.Args[endpointURL]
	if endpointURL == sideTreeURL {
		url += "identifiers"
	}

	httpVDR, err := httpbinding.New(url,
		httpbinding.WithAccept(func(method string) bool { return method == acceptDidMethod }))
	if err != nil {
		return fmt.Errorf("failed from httpbinding new ")
	}

	storeProv := a.getStoreProvider(agentID)

	loader, err := createJSONLDDocumentLoader(storeProv)
	if err != nil {
		return fmt.Errorf("create document loader: %w", err)
	}

	opts := append([]aries.Option{}, aries.WithStoreProvider(storeProv),
		aries.WithMessageServiceProvider(msgRegistrar), aries.WithVDR(httpVDR), aries.WithJSONLDDocumentLoader(loader))

	return a.create(agentID, inboundHost, inboundPort, scheme, opts...)
}

// CreateAgentWithHTTPDIDResolver creates agent with HTTP DID resolver.
//nolint:gocyclo
func (a *SDKSteps) CreateAgentWithHTTPDIDResolver(
	agents, inboundHost, inboundPort, endpointURL, acceptDidMethod string) error {
	var opts []aries.Option

	for _, agentID := range strings.Split(agents, ",") {
		opts = nil

		url := a.bddContext.Args[endpointURL]
		if endpointURL == sideTreeURL {
			url += "identifiers"
		}

		httpVDR, err := httpbinding.New(url,
			httpbinding.WithAccept(func(method string) bool { return method == acceptDidMethod }))
		if err != nil {
			return fmt.Errorf("failed from httpbinding new ")
		}

		storeProv := a.getStoreProvider(agentID)

		loader, err := createJSONLDDocumentLoader(storeProv)
		if err != nil {
			return fmt.Errorf("create document loader: %w", err)
		}

		opts = append(opts, aries.WithVDR(httpVDR), aries.WithStoreProvider(storeProv),
			aries.WithJSONLDDocumentLoader(loader))

		//nolint:nestif
		if g, ok := a.bddContext.Agents[agentID]; ok {
			ctx, err := g.Context()
			if err != nil {
				return fmt.Errorf("get agentID context: %w", err)
			}

			opts = append(opts, aries.WithKeyType(ctx.KeyType()), aries.WithKeyAgreementType(ctx.KeyAgreementType()),
				aries.WithMediaTypeProfiles(ctx.MediaTypeProfiles()))
		} else {
			if string(a.newKeyType) != "" {
				opts = append(opts, aries.WithKeyType(a.newKeyType))
			}

			if string(a.newKeyAgreementType) != "" {
				opts = append(opts, aries.WithKeyAgreementType(a.newKeyAgreementType))
			}

			if len(a.newMediaTypeProfiles) > 0 {
				opts = append(opts, aries.WithMediaTypeProfiles(a.newMediaTypeProfiles))
			}
		}

		if err := a.create(agentID, inboundHost, inboundPort, "http", opts...); err != nil {
			return err
		}
	}

	return nil
}

func (a *SDKSteps) getStoreProvider(agentID string) storage.Provider {
	storeProv := leveldb.NewProvider(dbPath + "/" + agentID + uuid.New().String())
	return storeProv
}

func (a *SDKSteps) createEdgeAgent(agentID, scheme, routeOpt string) error {
	return a.createEdgeAgentByDIDCommVer(agentID, scheme, routeOpt, false)
}

func (a *SDKSteps) createEdgeAgentByDIDCommV2(agentID, scheme, routeOpt string) error {
	return a.createEdgeAgentByDIDCommVer(agentID, scheme, routeOpt, true)
}

func (a *SDKSteps) createEdgeAgentByDIDCommVer(agentID, scheme, routeOpt string, useDIDCommV2 bool) error {
	var opts []aries.Option

	storeProv := a.getStoreProvider(agentID)

	if routeOpt != decorator.TransportReturnRouteAll {
		return errors.New("only 'all' transport route return option is supported")
	}

	loader, err := createJSONLDDocumentLoader(storeProv)
	if err != nil {
		return fmt.Errorf("create document loader: %w", err)
	}

	opts = append(opts,
		aries.WithStoreProvider(storeProv),
		aries.WithTransportReturnRoute(routeOpt),
		aries.WithJSONLDDocumentLoader(loader),
	)

	if useDIDCommV2 {
		opts = append(opts, aries.WithMediaTypeProfiles([]string{transport.MediaTypeDIDCommV2Profile}))
	}

	sch := strings.Split(scheme, ",")

	for _, s := range sch {
		switch s {
		case webSocketTransportProvider:
			opts = append(opts, aries.WithOutboundTransports(ws.NewOutbound()))
		case httpTransportProvider:
			out, err := arieshttp.NewOutbound(arieshttp.WithOutboundHTTPClient(&http.Client{}))
			if err != nil {
				return fmt.Errorf("failed to create http outbound: %w", err)
			}

			opts = append(opts, aries.WithOutboundTransports(ws.NewOutbound(), out))
		default:
			return fmt.Errorf("invalid transport provider type : %s (only websocket/http is supported)", scheme)
		}
	}

	return a.createFramework(agentID, opts...)
}

//nolint: gocyclo
func (a *SDKSteps) create(agentID, inboundHosts, inboundPorts, schemes string, opts ...aries.Option) error {
	const (
		portAttempts  = 5
		listenTimeout = 2 * time.Second
	)

	scheme := strings.Split(schemes, ",")
	hosts := strings.Split(inboundHosts, ",")
	ports := strings.Split(inboundPorts, ",")
	schemeAddrMap := make(map[string]string)

	for i := 0; i < len(scheme); i++ {
		port := ports[i]
		if port == "random" {
			port = strconv.Itoa(mustGetRandomPort(portAttempts))
		}

		inboundAddr := fmt.Sprintf("%s:%s", hosts[i], port)

		schemeAddrMap[scheme[i]] = inboundAddr
	}

	for _, s := range scheme {
		switch s {
		case webSocketTransportProvider:
			inbound, err := ws.NewInbound(schemeAddrMap[s], "ws://"+schemeAddrMap[s], "", "")
			if err != nil {
				return fmt.Errorf("failed to create websocket: %w", err)
			}

			opts = append(opts, aries.WithInboundTransport(inbound), aries.WithOutboundTransports(ws.NewOutbound()))
		case httpTransportProvider:
			opts = append(opts, defaults.WithInboundHTTPAddr(schemeAddrMap[s], "http://"+schemeAddrMap[s], "", ""))

			out, err := arieshttp.NewOutbound(arieshttp.WithOutboundHTTPClient(&http.Client{}))
			if err != nil {
				return fmt.Errorf("failed to create http outbound: %w", err)
			}

			opts = append(opts, aries.WithOutboundTransports(ws.NewOutbound(), out))
		default:
			return fmt.Errorf("invalid transport provider type : %s (only websocket/http is supported)", scheme)
		}
	}

	err := a.createFramework(agentID, opts...)
	if err != nil {
		return fmt.Errorf("failed to create new agent: %w", err)
	}

	for _, inboundAddr := range schemeAddrMap {
		if err := listenFor(inboundAddr, listenTimeout); err != nil {
			return err
		}

		logger.Debugf("Agent %s start listening on %s", agentID, inboundAddr)
	}

	return nil
}

func (a *SDKSteps) createFramework(agentID string, opts ...aries.Option) error {
	agent, err := aries.New(opts...)
	if err != nil {
		return fmt.Errorf("failed to create new agent: %w", err)
	}

	ctx, err := agent.Context()
	if err != nil {
		return fmt.Errorf("failed to create context: %w", err)
	}

	a.bddContext.Agents[agentID] = agent
	a.bddContext.AgentCtx[agentID] = ctx
	a.bddContext.Messengers[agentID] = agent.Messenger()

	return nil
}

type provider struct {
	ContextStore        ldstore.ContextStore
	RemoteProviderStore ldstore.RemoteProviderStore
}

func (p *provider) JSONLDContextStore() ldstore.ContextStore {
	return p.ContextStore
}

func (p *provider) JSONLDRemoteProviderStore() ldstore.RemoteProviderStore {
	return p.RemoteProviderStore
}

func createJSONLDDocumentLoader(storageProvider storage.Provider) (jsonld.DocumentLoader, error) {
	contextStore, err := ldstore.NewContextStore(cachedstore.NewProvider(storageProvider, mem.NewProvider()))
	if err != nil {
		return nil, fmt.Errorf("create JSON-LD context store: %w", err)
	}

	remoteProviderStore, err := ldstore.NewRemoteProviderStore(storageProvider)
	if err != nil {
		return nil, fmt.Errorf("create remote provider store: %w", err)
	}

	p := &provider{
		ContextStore:        contextStore,
		RemoteProviderStore: remoteProviderStore,
	}

	loader, err := ld.NewDocumentLoader(p, ld.WithExtraContexts(bddldcontext.Extra()...))
	if err != nil {
		return nil, err
	}

	return loader, nil
}

// SetContext is called before every scenario is run with a fresh new context.
func (a *SDKSteps) SetContext(ctx *context.BDDContext) {
	a.bddContext = ctx

	a.didExchangeSDKS = didexchangebdd.NewDIDExchangeSDKSteps()
	a.didExchangeSDKS.SetContext(ctx)
}

// RegisterSteps registers agent steps.
func (a *SDKSteps) RegisterSteps(s *godog.Suite) {
	s.Step(`^"([^"]*)" agent is running on "([^"]*)" port "([^"]*)" with "([^"]*)" as the transport provider$`,
		a.CreateAgent)
	s.Step(`^"([^"]*)" agent is running on "([^"]*)" port "([^"]*)" with "([^"]*)" using DIDCommV2 as `+
		`the transport provider$`,
		a.createAgentByDIDCommV2)
	s.Step(`^"([^"]*)" exchange DIDs V2 with "([^"]*)"$`, a.createConnectionV2)
	s.Step(`^"([^"]*)" agent is running on "([^"]*)" port "([^"]*)" with "([^"]*)" as the transport provider `+
		`using webkms with key server at "([^"]*)" URL, using "([^"]*)" controller`, a.CreateAgentWithRemoteKMS)
	s.Step(`^"([^"]*)" edge agent is running with "([^"]*)" as the outbound transport provider `+
		`and "([^"]*)" as the transport return route option`, a.createEdgeAgent)
	s.Step(`^"([^"]*)" edge agent is running with "([^"]*)" as the outbound transport provider `+
		`and "([^"]*)" using DIDCommV2 as the transport return route option`, a.createEdgeAgentByDIDCommV2)
	s.Step(`^"([^"]*)" agent is running on "([^"]*)" port "([^"]*)" `+
		`with http-binding did resolver url "([^"]*)" which accepts did method "([^"]*)"$`, a.CreateAgentWithHTTPDIDResolver)
	s.Step(`^"([^"]*)" agent with message registrar is running on "([^"]*)" port "([^"]*)" `+
		`with "([^"]*)" as the transport provider$`, a.createAgentWithRegistrar)
	s.Step(`^"([^"]*)" agent with message registrar is running on "([^"]*)" port "([^"]*)" with "([^"]*)" `+
		`as the transport provider and http-binding did resolver url "([^"]*)" which accepts did method "([^"]*)"$`,
		a.createAgentWithRegistrarAndHTTPDIDResolver)
	s.Step(`^options ""([^"]*)"" ""([^"]*)"" ""([^"]*)""$`, a.scenario)
	s.Step(`^all agents are using Media Type Profiles "([^"]*)"$`, a.useMediaTypeProfiles)
}

func mustGetRandomPort(n int) int {
	for ; n > 0; n-- {
		port, err := getRandomPort()
		if err != nil {
			continue
		}

		return port
	}

	panic("cannot acquire the random port")
}

func getRandomPort() (int, error) {
	const network = "tcp"

	addr, err := net.ResolveTCPAddr(network, "localhost:0")
	if err != nil {
		return 0, err
	}

	listener, err := net.ListenTCP(network, addr)
	if err != nil {
		return 0, err
	}

	if err := listener.Close(); err != nil {
		return 0, err
	}

	return listener.Addr().(*net.TCPAddr).Port, nil
}

func listenFor(host string, d time.Duration) error {
	timeout := time.After(d)

	for {
		select {
		case <-timeout:
			return errors.New("timeout: server is not available")
		default:
			conn, err := net.Dial("tcp", host)
			if err != nil {
				continue
			}

			return conn.Close()
		}
	}
}
