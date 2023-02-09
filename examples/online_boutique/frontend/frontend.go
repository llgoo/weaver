// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package frontend

import (
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ServiceWeaver/weaver"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/adservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/cartservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/checkoutservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/currencyservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/productcatalogservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/recommendationservice"
	"github.com/ServiceWeaver/weaver/examples/online_boutique/shippingservice"
	"github.com/gorilla/mux"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	cookieMaxAge = 60 * 60 * 48

	cookiePrefix    = "shop_"
	cookieSessionID = cookiePrefix + "session-id"
	cookieCurrency  = cookiePrefix + "currency"
)

var (
	//go:embed static/*
	staticFS embed.FS

	validEnvs = []string{"local", "gcp"}
)

type platformDetails struct {
	css      string
	provider string
}

func (plat *platformDetails) setPlatformDetails(env string) {
	if env == "gcp" {
		plat.provider = "Google Cloud"
		plat.css = "gcp-platform"
	} else {
		plat.provider = "local"
		plat.css = "local"
	}
}

// Server is the application frontend.
type Server struct {
	handler  http.Handler
	root     weaver.Instance
	platform platformDetails
	hostname string

	catalogService        productcatalogservice.T
	currencyService       currencyservice.T
	cartService           cartservice.T
	recommendationService recommendationservice.T
	checkoutService       checkoutservice.T
	shippingService       shippingservice.T
	adService             adservice.T
}

// NewServer returns the new application frontend.
func NewServer(root weaver.Instance) (*Server, error) {
	// Setup the services.
	catalogService, err := weaver.Get[productcatalogservice.T](root)
	if err != nil {
		return nil, err
	}
	currencyService, err := weaver.Get[currencyservice.T](root)
	if err != nil {
		return nil, err
	}
	cartService, err := weaver.Get[cartservice.T](root)
	if err != nil {
		return nil, err
	}
	recommendationService, err := weaver.Get[recommendationservice.T](root)
	if err != nil {
		return nil, err
	}
	checkoutService, err := weaver.Get[checkoutservice.T](root)
	if err != nil {
		return nil, err
	}
	shippingService, err := weaver.Get[shippingservice.T](root)
	if err != nil {
		return nil, err
	}
	adService, err := weaver.Get[adservice.T](root)
	if err != nil {
		return nil, err
	}

	// Find out where we're running.
	// Set ENV_PLATFORM (default to local if not set; use env var if set;
	// otherwise detect GCP, which overrides env).
	var env = os.Getenv("ENV_PLATFORM")
	// Only override from env variable if set + valid env
	if env == "" || !stringinSlice(validEnvs, env) {
		fmt.Println("env platform is either empty or invalid")
		env = "local"
	}
	// Autodetect GCP
	addrs, err := net.LookupHost("metadata.google.internal.")
	if err == nil && len(addrs) >= 0 {
		root.Logger().Debug("Detected Google metadata server, setting ENV_PLATFORM to GCP.", "address", addrs)
		env = "gcp"
	}
	root.Logger().Debug("ENV_PLATFORM", "platform", env)
	platform := platformDetails{}
	platform.setPlatformDetails(strings.ToLower(env))
	hostname, err := os.Hostname()
	if err != nil {
		root.Logger().Debug(`cannot get hostname for frontend: using "unknown"`)
		hostname = "unknown"
	}

	// Create the server.
	s := &Server{
		root:                  root,
		platform:              platform,
		hostname:              hostname,
		catalogService:        catalogService,
		currencyService:       currencyService,
		cartService:           cartService,
		recommendationService: recommendationService,
		checkoutService:       checkoutService,
		shippingService:       shippingService,
		adService:             adService,
	}

	// Setup the handler.
	staticHTML, err := fs.Sub(fs.FS(staticFS), "static")
	if err != nil {
		return nil, err
	}
	r := mux.NewRouter()

	// Helper that adds a handler with HTTP metric instrumentation.
	handleInstrumented := func(path, label string, fn func(http.ResponseWriter, *http.Request)) *mux.Route {
		return r.Handle(path, weaver.InstrumentHandler(label, http.HandlerFunc(fn)))
	}

	handleInstrumented("/", "home", s.homeHandler).Methods(http.MethodGet, http.MethodHead)
	handleInstrumented("/product/{id}", "product", s.productHandler).Methods(http.MethodGet, http.MethodHead)
	handleInstrumented("/cart", "cart_view", s.viewCartHandler).Methods(http.MethodGet, http.MethodHead)
	handleInstrumented("/cart", "cart_add", s.addToCartHandler).Methods(http.MethodPost)
	handleInstrumented("/cart/empty", "cart_empty", s.emptyCartHandler).Methods(http.MethodPost)
	handleInstrumented("/setCurrency", "setcurrency", s.setCurrencyHandler).Methods(http.MethodPost)
	handleInstrumented("/logout", "logout", s.logoutHandler).Methods(http.MethodGet)
	handleInstrumented("/cart/checkout", "cart_checkout", s.placeOrderHandler).Methods(http.MethodPost)
	r.PathPrefix("/static/").Handler(weaver.InstrumentHandler("static", http.StripPrefix("/static/", http.FileServer(http.FS(staticHTML)))))
	handleInstrumented("/robots.txt", "robots", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /") })

	// No instrumentation of /healthz
	r.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })

	// Set handler and return.
	var handler http.Handler = r
	// TODO(spetrovic): Use the Service Weaver per-component config to provisionaly
	// add these stats.
	handler = ensureSessionID(handler)             // add session ID
	handler = newLogHandler(root, handler)         // add logging
	handler = otelhttp.NewHandler(handler, "http") // add tracing
	s.handler = handler

	return s, nil
}

func (s *Server) Run(localAddr string) error {
	lis, err := s.root.Listener("boutique", weaver.ListenerOptions{LocalAddress: localAddr})
	if err != nil {
		return err
	}
	s.root.Logger().Debug("Frontend available", "addr", lis)
	return http.Serve(lis, s.handler)
}