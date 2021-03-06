// Copyright 2020 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"istio.io/istio/security/pkg/stsservice/tokenmanager/google"

	proxyEnv "istio.io/istio/mixer/test/client/env"
	istioEnv "istio.io/istio/pkg/test/env"
	xdsService "istio.io/istio/security/pkg/stsservice/mock"
	stsServer "istio.io/istio/security/pkg/stsservice/server"
	"istio.io/istio/security/pkg/stsservice/tokenmanager"
	tokenBackend "istio.io/istio/security/pkg/stsservice/tokenmanager/google/mock"
)

const (
	// Paths to credentials which will be loaded by proxy. These paths should
	// match bootstrap config in testdata/bootstrap.yaml
	certPath       = "/tmp/sts-ca-certificates.crt"
	proxyTokenPath = "/tmp/sts-envoy-token.jwt"
)

type Env struct {
	ProxySetUp *proxyEnv.TestSetup
	AuthServer *tokenBackend.AuthorizationServer

	stsServer         *stsServer.Server
	xDSServer         *grpc.Server
	ProxyListenerPort int
	initialToken      string // initial token is sent to STS server for token exchange
}

func (e *Env) TearDown() {
	// Stop proxy first, otherwise XDS stream is still alive and server's graceful
	// stop will be blocked.
	e.ProxySetUp.TearDown()
	_ = e.AuthServer.Stop()
	e.xDSServer.GracefulStop()
	e.stsServer.Stop()
}

func getDataFromFile(filePath string, t *testing.T) string {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read %q", filePath)
	}
	return string(data)
}

// WriteDataToFile writes data into file
func WriteDataToFile(path string, content string) error {
	if path == "" {
		return errors.New("empty file path")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(content); err != nil {
		return err
	}
	_ = f.Sync()
	return nil
}

// SetUpTest starts Envoy, XDS server, STS server, token manager, and a token service backend.
// Envoy loads a test config that requires token credential to access XDS server.
// That token credential is provisioned by STS server.
// Here is a map between ports and servers
// auth server            : MixerPort
// STS server             : ServerProxyPort
// Dynamic proxy listener : ClientProxyPort
// Static proxy listener  : TCPProxyPort
// XDS server             : DiscoveryPort
// test backend           : BackendPort
// proxy admin            : AdminPort
func SetUpTest(t *testing.T, cb *xdsService.XDSCallbacks, testID uint16) *Env {
	// Set up credential files for bootstrap config
	jwtToken := getDataFromFile(istioEnv.IstioSrc+"/security/pkg/stsservice/test/testdata/trustworthy-jwt.jwt", t)
	if err := WriteDataToFile(proxyTokenPath, jwtToken); err != nil {
		t.Fatalf("failed to set up token file %s: %v", proxyTokenPath, err)
	}
	caCert := getDataFromFile(istioEnv.IstioSrc+"/security/pkg/stsservice/test/testdata/ca-certificate.crt", t)
	if err := WriteDataToFile(certPath, caCert); err != nil {
		t.Fatalf("failed to set up ca certificate file %s: %v", certPath, err)
	}

	env := &Env{
		initialToken: jwtToken,
	}
	// Set up test environment for Proxy
	proxySetUp := proxyEnv.NewTestSetup(testID, t)
	proxySetUp.SetNoMixer(true)
	proxySetUp.EnvoyTemplate = getDataFromFile(istioEnv.IstioSrc+"/security/pkg/stsservice/test/testdata/bootstrap.yaml", t)
	env.ProxySetUp = proxySetUp
	env.DumpPortMap(t)
	// Set up auth server that provides token service
	backend, err := tokenBackend.StartNewServer(t, tokenBackend.Config{
		SubjectToken: jwtToken,
		Port:         int(proxySetUp.Ports().MixerPort),
		AccessToken:  cb.ExpectedToken(),
	})
	if err != nil {
		t.Fatalf("failed to start a auth backend: %v", err)
	}
	env.AuthServer = backend

	// Set up STS server
	stsServer, err := setUpSTS(int(proxySetUp.Ports().ServerProxyPort), backend.URL)
	if err != nil {
		t.Fatalf("failed to start a STS server: %v", err)
	}
	env.stsServer = stsServer

	// Make sure STS server and auth backend are running
	env.WaitForStsFlowReady(t)

	// Set up XDS server
	env.ProxyListenerPort = int(proxySetUp.Ports().ClientProxyPort)
	ls := &xdsService.DynamicListener{Port: env.ProxyListenerPort}
	xds, err := xdsService.StartXDSServer(
		xdsService.XDSConf{Port: int(proxySetUp.Ports().DiscoveryPort),
			CertFile: istioEnv.IstioSrc + "/security/pkg/stsservice/test/testdata/server-certificate.crt",
			KeyFile:  istioEnv.IstioSrc + "/security/pkg/stsservice/test/testdata/server-key.key"}, cb, ls, true)
	if err != nil {
		t.Fatalf("failed to start XDS server: %v", err)
	}
	env.xDSServer = xds

	return env
}

// DumpPortMap dumps port allocation status
// auth server            : MixerPort
// STS server             : ServerProxyPort
// Dynamic proxy listener : ClientProxyPort
// Static proxy listener  : TCPProxyPort
// XDS server             : DiscoveryPort
// test backend           : BackendPort
// proxy admin            : AdminPort
func (e *Env) DumpPortMap(t *testing.T) {
	log.Printf("\n\tport allocation status\t\t\t\n"+
		"auth server\t\t:\t%d\n"+
		"STS server\t\t:\t%d\n"+
		"dynamic listener port\t:\t%d\n"+
		"static listener port\t:\t%d\n"+
		"XDS server\t\t:\t%d\n"+
		"test backend\t\t:\t%d\n"+
		"proxy admin\t\t:\t%d", e.ProxySetUp.Ports().MixerPort,
		e.ProxySetUp.Ports().ServerProxyPort, e.ProxySetUp.Ports().ClientProxyPort,
		e.ProxySetUp.Ports().TCPProxyPort, e.ProxySetUp.Ports().DiscoveryPort,
		e.ProxySetUp.Ports().BackendPort, e.ProxySetUp.Ports().AdminPort)
}

func (e *Env) StartProxy(t *testing.T) {
	if err := e.ProxySetUp.SetUp(); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	log.Println("proxy is running...")
}

// WaitForStsFlowReady sends STS requests to STS server using HTTP client, and
// verifies that the STS flow is ready.
func (e *Env) WaitForStsFlowReady(t *testing.T) {
	t.Logf("%s check if all servers in the STS flow are up and ready", time.Now().String())
	addr, _ := net.ResolveTCPAddr("tcp", fmt.Sprintf("127.0.0.1:%d", e.ProxySetUp.Ports().ServerProxyPort))
	stsServerAddress := addr.String()
	hTTPClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				t.Logf("set up server address to dial %s", addr)
				addr = stsServerAddress
				return net.Dial(network, addr)
			},
		},
	}
	// keep sending requests periodically until a success STS response is received
	req := e.genStsReq(stsServerAddress)
	for i := 0; i < 20; i++ {
		resp, err := hTTPClient.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Type") == "application/json" {
				t.Logf("%s all servers in the STS flow are up and ready", time.Now().String())
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("STS flow is not ready")
}

func (e *Env) genStsReq(stsAddr string) (req *http.Request) {
	stsQuery := url.Values{}
	stsQuery.Set("grant_type", stsServer.TokenExchangeGrantType)
	stsQuery.Set("resource", "https//:backend.example.com")
	stsQuery.Set("audience", "audience")
	stsQuery.Set("scope", "https://www.googleapis.com/auth/cloud-platform")
	stsQuery.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	stsQuery.Set("subject_token", e.initialToken)
	stsQuery.Set("subject_token_type", stsServer.SubjectTokenType)
	stsQuery.Set("actor_token", "")
	stsQuery.Set("actor_token_type", "")
	stsURL := "http://" + stsAddr + stsServer.TokenPath
	req, _ = http.NewRequest("POST", stsURL, strings.NewReader(stsQuery.Encode()))
	req.Header.Set("Content-Type", stsServer.URLEncodedForm)
	return req
}

func setUpSTS(stsPort int, backendURL string) (*stsServer.Server, error) {
	// Create token exchange Google plugin
	tokenExchangePlugin, _ := google.CreateTokenManagerPlugin(tokenBackend.FakeTrustDomain, tokenBackend.FakeProjectNum)
	federatedTokenTestingEndpoint := backendURL + "/v1/identitybindingtoken"
	accessTokenTestingEndpoint := backendURL + "/v1/projects/-/serviceAccounts/service-%s@gcp-sa-meshdataplane.iam.gserviceaccount.com:generateAccessToken"
	tokenExchangePlugin.SetEndpoints(federatedTokenTestingEndpoint, accessTokenTestingEndpoint)
	// Create token manager
	tm := tokenmanager.CreateTokenManager(tokenmanager.GoogleTokenExchange,
		tokenmanager.Config{TrustDomain: tokenBackend.FakeTrustDomain})
	tm.(*tokenmanager.TokenManager).SetPlugin(tokenExchangePlugin)
	// Create STS server
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("127.0.0.1:%d", stsPort))
	if err != nil {
		return nil, fmt.Errorf("failed to create address %v", err)
	}
	return stsServer.NewServer(stsServer.Config{LocalHostAddr: addr.IP.String(), LocalPort: addr.Port}, tm)
}
