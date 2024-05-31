package obligator

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/lestrrat-go/jwx/v2/jwt/openid"
)

type OAuth2ServerMetadata struct {
	Issuer                            string   `json:"issuer,omitempty"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint                     string   `json:"token_endpoint,omitempty"`
	UserinfoEndpoint                  string   `json:"userinfo_endpoint,omitempty"`
	JwksUri                           string   `json:"jwks_uri,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported,omitempty"`
	IdTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	SubjectTypesSupported             []string `json:"subject_types_supported"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	IntrospectionEndpoint             string   `json:"introspection_endpoint,omitempty"`
}

type OAuth2AuthRequest struct {
	ClientId      string `json:"client_id"`
	RedirectUri   string `json:"redirect_uri"`
	Scope         string `json:"scope"`
	State         string `json:"state"`
	ResponseType  string `json:"response_type"`
	CodeChallenge string `json:"code_challenge"`
}

type OIDCHandler struct {
	mux *http.ServeMux
}

type OIDCRegistrationResponse struct {
	ClientId string `json:"client_id"`
}

type OIDCRegistrationRequest struct {
	RedirectUris []string `json:"redirect_uris"`
}

func NewOIDCHandler(storage Storage, tmpl *template.Template) *OIDCHandler {
	mux := http.NewServeMux()

	h := &OIDCHandler{
		mux: mux,
	}

	publicJwks, err := jwk.PublicSetOf(storage.GetJWKSet())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	prefix := storage.GetPrefix()
	loginKeyName := prefix + "login_key"

	// draft-ietf-oauth-security-topics-24 2.6
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")

		rootUri := storage.GetRootUri()

		doc := OAuth2ServerMetadata{
			Issuer:                           rootUri,
			AuthorizationEndpoint:            fmt.Sprintf("%s/auth", rootUri),
			TokenEndpoint:                    fmt.Sprintf("%s/token", rootUri),
			UserinfoEndpoint:                 fmt.Sprintf("%s/userinfo", rootUri),
			JwksUri:                          fmt.Sprintf("%s/jwks", rootUri),
			ScopesSupported:                  []string{"openid", "email", "profile"},
			ResponseTypesSupported:           []string{"code"},
			IdTokenSigningAlgValuesSupported: []string{"RS256"},
			// draft-ietf-oauth-security-topics-24 2.1.1
			CodeChallengeMethodsSupported: []string{"S256"},
			// https://openid.net/specs/openid-connect-core-1_0.html#SubjectIDTypes
			SubjectTypesSupported:             []string{"public"},
			RegistrationEndpoint:              fmt.Sprintf("%s/register", rootUri),
			TokenEndpointAuthMethodsSupported: []string{"none"},
		}

		json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(publicJwks)
	})

	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {

		var regReq OIDCRegistrationRequest

		err = json.NewDecoder(r.Body).Decode(&regReq)
		if err != nil {
			w.WriteHeader(400)
			return
		}

		if len(regReq.RedirectUris) == 0 {
			w.WriteHeader(400)
			io.WriteString(w, "Need at least one redirect_uri")
			return
		}

		parsedClientIdUrl, err := url.Parse(regReq.RedirectUris[0])
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		clientId := fmt.Sprintf("https://%s", parsedClientIdUrl.Host)

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(201)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		resp := OIDCRegistrationResponse{
			ClientId: clientId,
		}

		enc.Encode(resp)
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		parts := strings.Split(authHeader, " ")

		if len(parts) != 2 {
			w.WriteHeader(400)
			io.WriteString(w, "Invalid Authorization header")
			return
		}

		accessToken := parts[1]

		parsed, err := jwt.Parse([]byte(accessToken), jwt.WithKeySet(publicJwks))
		if err != nil {
			w.WriteHeader(401)
			io.WriteString(w, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")

		userResponse := UserinfoResponse{
			Sub:   parsed.Subject(),
			Email: parsed.Subject(),
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(userResponse)
	})

	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		ar, err := ParseAuthRequest(w, r)
		if err != nil {
			return
		}

		identities := []*Identity{}
		logins := make(map[string][]*Login)

		var hashedLoginKey string

		loginKeyCookie, err := r.Cookie(loginKeyName)
		if err == nil && loginKeyCookie.Value != "" {
			hashedLoginKey = Hash(loginKeyCookie.Value)

			parsed, err := jwt.Parse([]byte(loginKeyCookie.Value), jwt.WithKeySet(publicJwks))
			if err != nil {
				// Only add identities from current cookie if it's valid
			} else {
				tokIdentsInterface, exists := parsed.Get("identities")
				if exists {
					if tokIdents, ok := tokIdentsInterface.([]*Identity); ok {
						identities = tokIdents
					}
				}

				tokLoginsInterface, exists := parsed.Get("logins")
				if exists {
					if tokLogins, ok := tokLoginsInterface.(map[string][]*Login); ok {
						logins = tokLogins
					}
				}
			}

		}

		previousLogins, ok := logins[ar.ClientId]
		if !ok {
			previousLogins = []*Login{}
		}

		sort.Slice(previousLogins, func(i, j int) bool {
			return previousLogins[i].Timestamp > previousLogins[j].Timestamp
		})

		remainingIdents := []*Identity{}
		for _, ident := range identities {
			found := false
			for _, login := range previousLogins {
				if login.Id == ident.Id && login.ProviderName == ident.ProviderName {
					found = true
					break
				}
			}
			if !found {
				remainingIdents = append(remainingIdents, ident)
			}
		}

		maxAge := 8 * time.Minute
		issuedAt := time.Now().UTC()
		authRequestJwt, err := jwt.NewBuilder().
			IssuedAt(issuedAt).
			Expiration(issuedAt.Add(maxAge)).
			Claim("login_key_hash", hashedLoginKey).
			Claim("client_id", ar.ClientId).
			Claim("redirect_uri", ar.RedirectUri).
			Claim("state", ar.State).
			Claim("scope", r.Form.Get("scope")).
			Claim("nonce", r.Form.Get("nonce")).
			Claim("pkce_code_challenge", r.Form.Get("code_challenge")).
			Claim("response_type", ar.ResponseType).
			Build()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		setJwtCookie(storage, authRequestJwt, prefix+"auth_request", maxAge, w, r)

		providers, err := storage.GetOAuth2Providers()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		canEmail := true
		if _, err := storage.GetSmtpConfig(); err != nil {
			canEmail = false
		}

		parsedClientId, err := url.Parse(ar.ClientId)
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		returnUri := fmt.Sprintf("%s?%s", r.URL.Path, r.URL.RawQuery)

		data := struct {
			RootUri             string
			DisplayName         string
			ClientId            string
			Identities          []*Identity
			RemainingIdentities []*Identity
			PreviousLogins      []*Login
			OAuth2Providers     []OAuth2Provider
			LogoMap             map[string]template.HTML
			URL                 string
			CanEmail            bool
			ReturnUri           string
		}{
			RootUri:             storage.GetRootUri(),
			DisplayName:         storage.GetDisplayName(),
			ClientId:            parsedClientId.Host,
			Identities:          identities,
			RemainingIdentities: remainingIdents,
			PreviousLogins:      previousLogins,
			OAuth2Providers:     providers,
			LogoMap:             providerLogoMap,
			CanEmail:            canEmail,
			ReturnUri:           returnUri,
		}

		setReturnUriCookie(storage, returnUri, w)

		err = tmpl.ExecuteTemplate(w, "auth.html", data)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}
	})

	mux.HandleFunc("/approve", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()

		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			io.WriteString(w, err.Error())
			return
		}

		loginKeyCookie, err := r.Cookie(loginKeyName)
		if err != nil {
			w.WriteHeader(401)
			io.WriteString(w, "Only logged-in users can access this endpoint")
			return
		}

		parsedLoginKey, err := jwt.Parse([]byte(loginKeyCookie.Value), jwt.WithKeySet(publicJwks))
		if err != nil {
			w.WriteHeader(401)
			io.WriteString(w, err.Error())
			return
		}

		clearCookie(storage, prefix+"auth_request", w)

		parsedAuthReq, err := getJwtFromCookie(prefix+"auth_request", storage, w, r)
		if err != nil {
			w.WriteHeader(401)
			io.WriteString(w, err.Error())
			return
		}

		identId := r.Form.Get("identity_id")

		var identity *Identity
		tokIdentsInterface, exists := parsedLoginKey.Get("identities")
		if exists {
			if tokIdents, ok := tokIdentsInterface.([]*Identity); ok {
				for _, ident := range tokIdents {
					if ident.Id == identId {
						identity = ident
						break
					}
				}
			}
		}

		if identity == nil {
			w.WriteHeader(403)
			io.WriteString(w, "You don't have permissions for this identity")
			return
		}

		clientId := claimFromToken("client_id", parsedAuthReq)

		newLogin := &Login{
			IdType:       "email",
			Id:           identity.Id,
			ProviderName: identity.ProviderName,
		}

		newLoginCookie, err := addLoginToCookie(storage, loginKeyCookie.Value, clientId, newLogin)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}
		http.SetCookie(w, newLoginCookie)

		scope := claimFromToken("scope", parsedAuthReq)
		scopeParts := strings.Split(scope, " ")
		emailRequested := false
		profileRequested := false
		for _, scopePart := range scopeParts {
			if scopePart == "email" {
				emailRequested = true
			}

			if scopePart == "profile" {
				profileRequested = true
			}
		}

		issuedAt := time.Now().UTC()
		expiresAt := issuedAt.Add(24 * time.Hour)

		idTokenBuilder := openid.NewBuilder().
			Subject(identId).
			Audience([]string{clientId}).
			Issuer(storage.GetRootUri()).
			IssuedAt(issuedAt).
			Expiration(expiresAt).
			Claim("nonce", claimFromToken("nonce", parsedAuthReq))

		if emailRequested {
			idTokenBuilder.Email(identity.Email).
				EmailVerified(identity.EmailVerified)
		}

		if profileRequested && identity.Name != "" {
			idTokenBuilder.Name(identity.Name)
		}

		idToken, err := idTokenBuilder.Build()
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		key, exists := storage.GetJWKSet().Key(0)
		if !exists {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "No keys available")
			return
		}

		signedIdToken, err := jwt.Sign(idToken, jwt.WithKey(jwa.RS256, key))
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		// TODO: should maybe be encrypting this
		codeJwt, err := jwt.NewBuilder().
			IssuedAt(issuedAt).
			Expiration(issuedAt.Add(16*time.Second)).
			Subject(idToken.Email()).
			Claim("id_token", string(signedIdToken)).
			Claim("pkce_code_challenge", claimFromToken("pkce_code_challenge", parsedAuthReq)).
			Build()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		signedCode, err := jwt.Sign(codeJwt, jwt.WithKey(jwa.RS256, key))
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		responseType := claimFromToken("response_type", parsedAuthReq)

		// https://openid.net/specs/oauth-v2-multiple-response-types-1_0.html#none
		if responseType == "none" {
			redirectUri := claimFromToken("redirect_uri", parsedAuthReq)
			http.Redirect(w, r, redirectUri, http.StatusSeeOther)
		} else {
			url := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&code=%s&state=%s&scope=%s",
				claimFromToken("redirect_uri", parsedAuthReq),
				claimFromToken("client_id", parsedAuthReq),
				claimFromToken("redirect_uri", parsedAuthReq),
				string(signedCode),
				claimFromToken("state", parsedAuthReq),
				claimFromToken("scope", parsedAuthReq))

			http.Redirect(w, r, url, http.StatusSeeOther)
		}
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		codeJwt := r.Form.Get("code")

		parsedCodeJwt, err := jwt.Parse([]byte(codeJwt), jwt.WithKeySet(publicJwks))
		if err != nil {
			fmt.Println(err.Error())
			w.WriteHeader(401)
			io.WriteString(w, err.Error())
			return
		}

		signedIdTokenIface, exists := parsedCodeJwt.Get("id_token")
		if !exists {
			w.WriteHeader(401)
			io.WriteString(w, "Invalid id_token in code")
			return
		}

		signedIdToken, ok := signedIdTokenIface.(string)
		if !ok {
			w.WriteHeader(401)
			io.WriteString(w, "Invalid id_token in code")
			return
		}

		pkceCodeChallenge, exists := parsedCodeJwt.Get("pkce_code_challenge")
		if !exists {
			w.WriteHeader(401)
			io.WriteString(w, "Invalid pkce_code_challenge in code")
			return
		}

		// https://datatracker.ietf.org/doc/html/draft-ietf-oauth-security-topics#section-4.8.2
		// draft-ietf-oauth-security-topics-24 2.1.1
		pkceCodeVerifier := r.Form.Get("code_verifier")
		if pkceCodeChallenge != "" {
			challenge := GeneratePKCECodeChallenge(pkceCodeVerifier)
			if challenge != pkceCodeChallenge {
				w.WriteHeader(401)
				io.WriteString(w, "Invalid code_verifier")
				return
			}
		} else {
			if pkceCodeVerifier != "" {
				w.WriteHeader(401)
				io.WriteString(w, "code_verifier provided for request that did not include code_challenge")
				return
			}
		}

		key, exists := storage.GetJWKSet().Key(0)
		if !exists {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "No keys available")
			return
		}

		issuedAt := time.Now().UTC()
		accessTokenJwt, err := jwt.NewBuilder().
			IssuedAt(issuedAt).
			Expiration(issuedAt.Add(16 * time.Second)).
			Subject(parsedCodeJwt.Subject()).
			Build()
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		signedAccessToken, err := jwt.Sign(accessTokenJwt, jwt.WithKey(jwa.RS256, key))
		if err != nil {
			w.WriteHeader(400)
			io.WriteString(w, err.Error())
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.Header().Set("Cache-Control", "no-store")

		tokenRes := OAuth2TokenResponse{
			AccessToken: string(signedAccessToken),
			ExpiresIn:   3600,
			IdToken:     string(signedIdToken),
			TokenType:   "bearer",
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(tokenRes)
	})

	return h
}

func ParseAuthRequest(w http.ResponseWriter, r *http.Request) (*OAuth2AuthRequest, error) {
	r.ParseForm()

	clientId := r.Form.Get("client_id")
	if clientId == "" {
		w.WriteHeader(400)
		io.WriteString(w, "client_id missing")
		return nil, errors.New("client_id missing")
	}

	redirectUri := r.Form.Get("redirect_uri")
	if redirectUri == "" {
		w.WriteHeader(400)
		io.WriteString(w, "redirect_uri missing")
		return nil, errors.New("redirect_uri missing")
	}

	parsedClientIdUri, err := url.Parse(clientId)
	if err != nil {
		w.WriteHeader(400)
		msg := "client_id is not a valid URI"
		io.WriteString(w, msg)
		return nil, errors.New(msg)
	}

	parsedRedirectUri, err := url.Parse(redirectUri)
	if err != nil {
		w.WriteHeader(400)
		msg := "redirect_uri is not a valid URI"
		io.WriteString(w, msg)
		return nil, errors.New(msg)
	}

	// draft-ietf-oauth-security-topics-24 4.1
	if parsedClientIdUri.Host != parsedRedirectUri.Host {
		w.WriteHeader(400)
		io.WriteString(w, "redirect_uri must be on the same domain as client_id")
		fmt.Println(redirectUri, clientId)
		return nil, errors.New("redirect_uri must be on the same domain as client_id")
	}

	scope := r.Form.Get("scope")
	state := r.Form.Get("state")

	promptParam := r.Form.Get("prompt")
	if promptParam == "none" {
		errUrl := fmt.Sprintf("%s?error=interaction_required&state=%s",
			redirectUri, state)
		http.Redirect(w, r, errUrl, http.StatusSeeOther)
		return nil, errors.New("interaction required")
	}

	responseType := r.Form.Get("response_type")
	if responseType == "" {
		errUrl := fmt.Sprintf("%s?error=unsupported_response_type&state=%s",
			redirectUri, state)
		http.Redirect(w, r, errUrl, http.StatusSeeOther)
		return nil, errors.New("unsupported_response_type")
	}

	pkceCodeChallenge := r.Form.Get("code_challenge")

	return &OAuth2AuthRequest{
		ClientId:      clientId,
		RedirectUri:   redirectUri,
		ResponseType:  responseType,
		Scope:         scope,
		State:         state,
		CodeChallenge: pkceCodeChallenge,
	}, nil
}

func (h *OIDCHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}
