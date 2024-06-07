package obligator

import (
	"crypto/rand"
	"crypto/rsa"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/ip2location/ip2location-go/v9"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

type Server struct {
	api     *Api
	Config  ServerConfig
	Mux     *ObligatorMux
	storage Storage
}

type ServerConfig struct {
	Port                   int
	AuthDomains            []string
	Prefix                 string
	StorageDir             string
	DatabaseDir            string
	ApiSocketDir           string
	BehindProxy            bool
	DisplayName            string
	GeoDbPath              string
	FedCm                  bool
	ForwardAuthPassthrough bool
}

type SmtpConfig struct {
	Server     string `json:"server,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Port       int    `json:"port,omitempty"`
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
}

type OAuth2TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IdToken     string `json:"id_token,omitempty"`
}

type ObligatorMux struct {
	behindProxy bool
	mux         *http.ServeMux
}

type UserinfoResponse struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

type Validation struct {
	Id     string `json:"id"`
	IdType string `json:"id_type"`
}

const RateLimitTime = 24 * time.Hour

// const RateLimitTime = 10 * time.Minute
const EmailValidationsPerTimeLimit = 12

func NewObligatorMux(behindProxy bool) *ObligatorMux {
	s := &ObligatorMux{
		behindProxy: behindProxy,
		mux:         http.NewServeMux(),
	}

	return s
}

func (s *ObligatorMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO: see if we can re-enable script-src none. Removed it for FedCM support
	//w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; script-src 'none'")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")

	timestamp := time.Now().Format(time.RFC3339)

	remoteIp, err := getRemoteIp(r, s.behindProxy)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}

	fmt.Println(fmt.Sprintf("%s\t%s\t%s\t%s\t%s", timestamp, remoteIp, r.Method, r.Host, r.URL.Path))
	s.mux.ServeHTTP(w, r)
}

func (s *ObligatorMux) Handle(p string, h http.Handler) {
	s.mux.Handle(p, h)
}

func (s *ObligatorMux) HandleFunc(p string, f func(w http.ResponseWriter, r *http.Request)) {
	s.mux.HandleFunc(p, f)
}

//go:embed templates assets
var fs embed.FS

func NewServer(conf ServerConfig) *Server {

	if conf.Port == 0 {
		conf.Port = 1616
	}

	if conf.Prefix == "" {
		conf.Prefix = "obligator"
	}

	if conf.DisplayName == "" {
		conf.DisplayName = "obligator"
	}

	var identsType []*Identity
	jwt.RegisterCustomField("identities", identsType)
	var loginsType map[string][]*Login
	jwt.RegisterCustomField("logins", loginsType)
	var idTokenType string
	jwt.RegisterCustomField("id_token", idTokenType)

	storagePath := filepath.Join(conf.StorageDir, conf.Prefix+"storage.json")
	storage, err := NewJsonStorage(storagePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if conf.Prefix != "obligator" || storage.GetPrefix() == "" {
		storage.SetPrefix(conf.Prefix)
	}

	prefix := storage.GetPrefix()

	//sqliteStorage, err := NewSqliteStorage(prefix + "storage.sqlite")
	//if err != nil {
	//	fmt.Fprintln(os.Stderr, err.Error())
	//	os.Exit(1)
	//}

	dbPath := filepath.Join(conf.DatabaseDir, prefix+"db.sqlite")
	db, err := NewDatabase(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	cluster := NewCluster()
	proxy := NewProxy("caddy", conf.Port)

	if conf.DisplayName != "obligator" {
		storage.SetDisplayName(conf.DisplayName)
	}

	domains, err := db.GetDomains()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}

	if len(domains) == 0 {
		fmt.Fprintln(os.Stderr, "WARNING: domains set")
	}

	for _, d := range domains {
		// TODO: was running this in goroutines, but not all the
		// domains were making it into Caddy. Need to make it work
		// in parallel
		err = proxy.AddDomain(d.Domain)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}

	// TODO: re-enable
	//conf.AuthDomains = append(conf.AuthDomains, rootUrl.Host)

	if conf.FedCm {
		storage.SetFedCmEnable(true)
	}

	if conf.ForwardAuthPassthrough {
		storage.SetForwardAuthPassthrough(true)
	}

	oauth2MetaMan := NewOAuth2MetadataManager(storage)
	err = oauth2MetaMan.Update()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	api, err := NewApi(storage, conf.ApiSocketDir, oauth2MetaMan)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if storage.GetJWKSet().Len() == 0 {
		key, err := GenerateJWK()
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}

		storage.AddJWKKey(key)
	}

	tmpl, err := template.ParseFS(fs, "templates/*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	mux := NewObligatorMux(conf.BehindProxy)

	var geoDb *ip2location.DB
	if conf.GeoDbPath != "" {
		geoDb, err = ip2location.OpenDB(conf.GeoDbPath)
		if err != nil {
			fmt.Println(err.Error())
			return nil
		}
	}

	handler := NewHandler(storage, conf, tmpl)
	mux.Handle("/", handler)

	oidcHandler := NewOIDCHandler(storage, tmpl)
	mux.Handle("/.well-known/openid-configuration", oidcHandler)
	mux.Handle("/jwks", oidcHandler)
	mux.Handle("/register", oidcHandler)
	mux.Handle("/userinfo", oidcHandler)
	mux.Handle("/auth", oidcHandler)
	mux.Handle("/approve", oidcHandler)
	mux.Handle("/token", oidcHandler)

	addIdentityOauth2Handler := NewAddIdentityOauth2Handler(storage, oauth2MetaMan)
	mux.Handle("/login-oauth2", addIdentityOauth2Handler)
	mux.Handle("/callback", addIdentityOauth2Handler)

	addIdentityEmailHandler := NewAddIdentityEmailHandler(storage, db, cluster, tmpl, conf.BehindProxy, geoDb)
	mux.Handle("/login-email", addIdentityEmailHandler)
	mux.Handle("/email-sent", addIdentityEmailHandler)
	mux.Handle("/magic", addIdentityEmailHandler)
	mux.Handle("/confirm-magic", addIdentityEmailHandler)
	mux.Handle("/complete-email-login", addIdentityEmailHandler)

	addIdentityGamlHandler := NewAddIdentityGamlHandler(storage, cluster, tmpl)
	mux.Handle("/login-gaml", addIdentityGamlHandler)
	mux.Handle("/gaml-code", addIdentityGamlHandler)
	mux.Handle("/complete-gaml-login", addIdentityGamlHandler)

	qrHandler := NewQrHandler(storage, cluster, tmpl)
	mux.Handle("/login-qr", qrHandler)
	mux.Handle("/qr", qrHandler)
	mux.Handle("/send", qrHandler)
	mux.Handle("/receive", qrHandler)

	indieAuthPrefix := "/indieauth"
	indieAuthHandler := NewIndieAuthHandler(storage, tmpl, indieAuthPrefix)
	mux.Handle("/users/", indieAuthHandler)
	mux.Handle("/.well-known/oauth-authorization-server", indieAuthHandler)
	mux.Handle(indieAuthPrefix+"/", http.StripPrefix(indieAuthPrefix, indieAuthHandler))

	publicJwks, err := jwk.PublicSetOf(storage.GetJWKSet())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	mux.HandleFunc("/domains", func(w http.ResponseWriter, r *http.Request) {

		data := struct {
			*commonData
		}{
			commonData: newCommonData(nil, storage, r),
		}

		err = tmpl.ExecuteTemplate(w, "domains.html", data)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}
	})

	mux.HandleFunc("/add-domain", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		// TODO: sanitize domain
		domain := r.Form.Get("domain")

		if domain == "" {
			w.WriteHeader(400)
			io.WriteString(w, "Missing domain")
			return
		}

		_, err := url.Parse(domain)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		ownerId := r.Form.Get("owner_id")

		idents, _ := getIdentities(storage, r, publicJwks)

		match := false
		for _, ident := range idents {
			if ident.Id == ownerId {
				match = true
				break
			}
		}

		if !match {
			w.WriteHeader(403)
			io.WriteString(w, "You don't own that ID")
			return
		}

		//cname, err := getAuthoritativeCNAME(r.Context(), domain)
		//if err != nil {
		//        w.WriteHeader(500)
		//        io.WriteString(w, err.Error())
		//        return
		//}

		//if cname != rootUrl.Host {
		//        fmt.Println(cname, rootUrl.Host)
		//        w.WriteHeader(400)
		//        io.WriteString(w, "CNAME != host")
		//        return
		//}

		err = db.AddDomain(domain, ownerId)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		err = proxy.AddDomain(domain)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}
	})

	if storage.GetFedCmEnabled() {
		fedCmLoginEndpoint := "/login-fedcm-auto"
		fedCmHandler := NewFedCmHandler(storage, fedCmLoginEndpoint)
		mux.Handle("/.well-known/web-identity", fedCmHandler)
		mux.Handle("/fedcm/", http.StripPrefix("/fedcm", fedCmHandler))

		addIdentityFedCmHandler := NewAddIdentityFedCmHandler(storage, tmpl)
		mux.Handle("/login-fedcm", addIdentityFedCmHandler)
		mux.Handle("/complete-login-fedcm", addIdentityFedCmHandler)
	}

	s := &Server{
		Config:  conf,
		Mux:     mux,
		api:     api,
		storage: storage,
	}

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Mux.ServeHTTP(w, r)
}

func (s *Server) Start() error {
	server := http.Server{
		Addr:    fmt.Sprintf(":%d", s.Config.Port),
		Handler: s.Mux,
	}

	fmt.Println("Running")

	err := server.ListenAndServe()
	if err != nil {
		fmt.Fprintf(os.Stderr, err.Error())
		return err
	}

	return nil
}

// TODO: re-enable
//func (s *Server) AuthUri(authReq *OAuth2AuthRequest) string {
//	return AuthUri(s.Config.RootUri+"/auth", authReq)
//}

func AuthUri(serverUri string, authReq *OAuth2AuthRequest) string {
	uri := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=%s&state=%s&scope=%s",
		serverUri, authReq.ClientId, authReq.RedirectUri,
		authReq.ResponseType, authReq.State, authReq.Scope)
	return uri
}

func (s *Server) AuthDomains() []string {
	return s.Config.AuthDomains
}

func (s *Server) SetOAuth2Provider(prov OAuth2Provider) error {
	return s.api.SetOAuth2Provider(prov)
}

func (s *Server) AddUser(user User) error {
	return s.api.AddUser(user)
}

func (s *Server) GetUsers() ([]User, error) {
	return s.api.GetUsers()
}

func (s *Server) Validate(r *http.Request) (*Validation, error) {
	return validate(s.storage, r)
}

func checkErrPassthrough(err error, passthrough bool) (*Validation, error) {
	if passthrough {
		return nil, nil
	} else {
		return nil, err
	}
}

func validate(storage Storage, r *http.Request) (*Validation, error) {
	r.ParseForm()

	passthrough := storage.GetForwardAuthPassthrough()

	loginKeyName := storage.GetPrefix() + "login_key"

	loginKeyCookie, err := r.Cookie(loginKeyName)
	if err != nil {
		return checkErrPassthrough(err, passthrough)
	}

	// TODO: don't generate publicJwks every time
	publicJwks, err := jwk.PublicSetOf(storage.GetJWKSet())
	if err != nil {
		return checkErrPassthrough(err, passthrough)
	}

	parsed, err := jwt.Parse([]byte(loginKeyCookie.Value), jwt.WithKeySet(publicJwks))
	if err != nil {
		return checkErrPassthrough(err, passthrough)
	}

	tokIdentsInterface, exists := parsed.Get("identities")
	if !exists {
		return checkErrPassthrough(errors.New("No identities"), passthrough)
	}

	tokIdents, ok := tokIdentsInterface.([]*Identity)
	if !ok {
		return checkErrPassthrough(errors.New("No identities"), passthrough)
	}

	// TODO: maybe return whole list of identities?
	ident := tokIdents[0]

	v := &Validation{
		IdType: ident.IdType,
		Id:     ident.Id,
	}

	return v, nil
}

func GenerateJWK() (jwk.Key, error) {
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	key, err := jwk.FromRaw(raw)
	if err != nil {
		return nil, err
	}

	if _, ok := key.(jwk.RSAPrivateKey); !ok {
		return nil, err
	}

	err = jwk.AssignKeyID(key)
	if err != nil {
		return nil, err
	}

	key.Set("alg", "RS256")

	//key.Set(jwk.KeyUsageKey, "sig")
	//keyset := jwk.NewSet()
	//keyset.Add(key)
	//return keyset, nil

	return key, nil
}
