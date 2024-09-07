package obligator

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	//"time"

	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/lestrrat-go/jwx/v2/jwt/openid"
)

type AddIdentityOauth2Handler struct {
	mux *http.ServeMux
}

var providerLogoMap map[string]template.HTML

func buildProviderLogoMap(db Database) {
	providerLogoMap = make(map[string]template.HTML)

	providers, err := db.GetOAuth2Providers()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	for _, provider := range providers {

		logoPath := fmt.Sprintf("assets/logo_%s.svg", provider.ID)
		logoBytes, err := fs.ReadFile(logoPath)
		if err != nil {
			logoBytes, err = fs.ReadFile("assets/logo_generic_openid.svg")
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
		}

		providerLogoMap[provider.ID] = template.HTML(logoBytes)
	}
}

func NewAddIdentityOauth2Handler(db Database, oauth2MetaMan *OAuth2MetadataManager, jose *JOSE) *AddIdentityOauth2Handler {
	mux := http.NewServeMux()

	h := &AddIdentityOauth2Handler{
		mux: mux,
	}

	httpClient := &http.Client{}

	buildProviderLogoMap(db)

	prefix, err := db.GetPrefix()
	checkErr(err)

	mux.HandleFunc("/login-oauth2", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		oauth2ProviderId := r.Form.Get("oauth2_provider_id")

		provider, err := db.GetOAuth2ProviderByID(oauth2ProviderId)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		scope := "openid email profile"
		if provider.Scope != "" {
			scope = provider.Scope
		}

		var authURL string
		if provider.OpenIDConnect {
			meta, err := oauth2MetaMan.GetMeta(provider.ID)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}
			authURL = meta.AuthorizationEndpoint
		} else {
			authURL = provider.AuthorizationURI
		}

		// TODO: replace GeneratePKCEData with offical oauth2 package
		pkceCodeChallenge, pkceCodeVerifier, err := GeneratePKCEData()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		// TODO: is state really doing anything now that it's just
		// stored in a JWT? Maybe it should be encrypted
		state, err := genRandomKey()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, "Failed to generate state")
			return
		}

		nonce, err := genRandomKey()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, "Failed to generate nonce")
			return
		}

		// TODO: consider encrypting this JWT to keep the PKCE code
		// verifier secret from the frontend, ie malicious browser
		// extensions.
		issuedAt := time.Now().UTC()
		maxAge := 8 * time.Minute
		reqJwt, err := NewJWTBuilder().
			IssuedAt(issuedAt).
			Expiration(issuedAt.Add(maxAge)).
			Claim("provider_id", provider.ID).
			Claim("state", state).
			Claim("nonce", nonce).
			Claim("pkce_code_verifier", pkceCodeVerifier).
			Build()
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		setJwtCookie(db, r.Host, reqJwt, prefix+"upstream_oauth2_request", maxAge, w, r)

		callbackUri := fmt.Sprintf("%s/callback", domainToUri(r.Host))

		clientId := domainToUri(r.Host)
		if provider.ClientID != "" {
			clientId = provider.ClientID
		}

		url := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&state=%s&scope=%s&response_type=code&code_challenge_method=S256&code_challenge=%s&nonce=%s&prompt=consent",
			authURL, clientId, callbackUri, state,
			scope, pkceCodeChallenge, nonce)

		http.Redirect(w, r, url, http.StatusSeeOther)
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		upstreamAuthReqCookie, err := r.Cookie(prefix + "upstream_oauth2_request")
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		parsedUpstreamAuthReq, err := ParseJWT(db, upstreamAuthReqCookie.Value)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		oauth2Provider, err := db.GetOAuth2ProviderByID(claimFromToken("provider_id", parsedUpstreamAuthReq))
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		providerCode := r.Form.Get("code")

		callbackUri := fmt.Sprintf("%s/callback", domainToUri(r.Host))

		body := url.Values{}
		body.Set("code", providerCode)
		body.Set("client_id", oauth2Provider.ClientID)
		body.Set("client_secret", oauth2Provider.ClientSecret)
		body.Set("redirect_uri", callbackUri)
		body.Set("grant_type", "authorization_code")
		body.Set("code_verifier", claimFromToken("pkce_code_verifier", parsedUpstreamAuthReq))

		var tokenEndpoint string
		if oauth2Provider.OpenIDConnect {
			if oauth2Provider.ID == "facebook" {
				// Facebook strangely appears to implement all of OIDC except the token endpoint...
				// TODO: rather than special-casing this for facebook, maybe just check if
				// oauth2Provider.TokenURI is blank
				tokenEndpoint = oauth2Provider.TokenURI
			} else {
				meta, err := oauth2MetaMan.GetMeta(oauth2Provider.ID)
				if err != nil {
					w.WriteHeader(500)
					io.WriteString(w, err.Error())
					return
				}
				tokenEndpoint = meta.TokenEndpoint
			}
		} else {
			tokenEndpoint = oauth2Provider.TokenURI
		}

		upstreamReq, err := http.NewRequest(http.MethodPost,
			tokenEndpoint,
			strings.NewReader(body.Encode()))
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "Creating request failed")
			return
		}

		upstreamReq.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		upstreamReq.Header.Add("Accept", "application/json")

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "Doing request failed")
			return
		}

		if resp.StatusCode != 200 {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "Request failed with invalid status")
			b, _ := io.ReadAll(resp.Body)
			fmt.Println(string(b))
			return
		}

		var tokenRes OAuth2TokenResponse

		err = json.NewDecoder(resp.Body).Decode(&tokenRes)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		var email string

		name := ""

		if oauth2Provider.OpenIDConnect {
			keyset, err := oauth2MetaMan.GetKeyset(oauth2Provider.ID)
			if err != nil {
				w.WriteHeader(500)
				fmt.Fprintf(os.Stderr, err.Error())
				return
			}

			providerOauth2Token, err := jwt.Parse([]byte(tokenRes.IdToken), jwt.WithKeySet(keyset), jwt.WithToken(openid.New()))
			if err != nil {
				w.WriteHeader(500)
				fmt.Fprintf(os.Stderr, err.Error())
				return
			}

			providerOidcToken, ok := providerOauth2Token.(openid.Token)
			if !ok {
				w.WriteHeader(500)
				fmt.Fprintf(os.Stderr, "Not a valid OpenId Connect token")
				return
			}

			nonceClaim, exists := providerOidcToken.Get("nonce")
			if !exists {
				w.WriteHeader(400)
				fmt.Fprintf(os.Stderr, "Nonce missing")
				return
			}

			nonce, ok := nonceClaim.(string)
			if !ok {
				w.WriteHeader(400)
				fmt.Fprintf(os.Stderr, "Invalid nonce format")
				return
			}

			if claimFromToken("nonce", parsedUpstreamAuthReq) != nonce {
				w.WriteHeader(403)
				fmt.Fprintf(os.Stderr, "Invalid nonce")
				return
			}

			email = providerOidcToken.Email()
			name = providerOidcToken.Name()
		} else {
			_, email, _ = GetProfile(oauth2Provider, tokenRes.AccessToken)
		}

		users, err := db.GetUsers()
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		returnUri, err := getReturnUriCookie(db, r)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		config, err := db.GetConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			w.WriteHeader(500)
			return
		}

		if !config.Public && !validUser(email, users) {
			redirUrl := fmt.Sprintf("%s/no-account?%s", domainToUri(r.Host), returnUri)
			http.Redirect(w, r, redirUrl, http.StatusSeeOther)
			return
		}

		deleteReturnUriCookie(r.Host, db, w)

		cookieValue := ""
		loginKeyCookie, err := getLoginCookie(db, r)
		if err == nil {
			cookieValue = loginKeyCookie.Value
		}

		newIdent := &Identity{
			IdType:        "email",
			Id:            email,
			ProviderName:  oauth2Provider.Name,
			Name:          name,
			Email:         email,
			EmailVerified: true,
		}

		cookie, err := addIdentToCookie(r.Host, db, cookieValue, newIdent, jose)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		err = setLoginCookie(w, cookie)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		redirUrl := fmt.Sprintf("%s", returnUri)

		clearCookie(r.Host, prefix+"upstream_oauth2_request", w)

		http.Redirect(w, r, redirUrl, http.StatusSeeOther)
	})

	return h
}

func (h *AddIdentityOauth2Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// Modified from https://chrisguitarguy.com/2022/12/07/oauth-pkce-with-go/
func GeneratePKCECodeVerifier() (string, error) {
	const chars string = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-._~"
	verifier := ""
	for i := 0; i < 64; i++ {
		randIndex, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		verifier += string(chars[randIndex.Int64()])
	}

	return verifier, nil
}

func GeneratePKCECodeChallenge(verifier string) string {
	sha2 := sha256.New()
	io.WriteString(sha2, verifier)
	return base64.RawURLEncoding.EncodeToString(sha2.Sum(nil))
}

func GeneratePKCEData() (string, string, error) {

	verifier, err := GeneratePKCECodeVerifier()
	if err != nil {
		return "", "", err
	}

	challenge := GeneratePKCECodeChallenge(verifier)

	return challenge, verifier, nil
}

type GitHubEmailResponse []*GitHubEmail

type GitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func GetProfile(provider *OAuth2Provider, accessToken string) (string, string, error) {
	httpClient := &http.Client{}

	switch provider.ID {
	case "github":
		req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user/emails", nil)
		if err != nil {
			return "", "", err
		}

		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", accessToken))

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", "", err
		}

		if resp.StatusCode != 200 {
			return "", "", errors.New("Bad status getting profile")
		}

		var profileResponse GitHubEmailResponse

		err = json.NewDecoder(resp.Body).Decode(&profileResponse)
		if err != nil {
			return "", "", err
		}

		for _, email := range profileResponse {
			if email.Primary {
				return provider.URI, email.Email, nil
			}
		}

	}

	return "", "", errors.New("Unknown GetProfile error")
}

func GetOidcConfiguration(baseUrl string) (*OAuth2ServerMetadata, error) {

	url := fmt.Sprintf("%s/.well-known/openid-configuration", baseUrl)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, errors.New("Invalid HTTP response")
	}

	var doc OAuth2ServerMetadata

	err = json.NewDecoder(resp.Body).Decode(&doc)
	if err != nil {
		return nil, err
	}

	return &doc, nil
}
