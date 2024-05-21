package obligator

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/lestrrat-go/jwx/v2/jwt/openid"
)

type AddIdentityFedCmHandler struct {
	mux *http.ServeMux
}

func NewAddIdentityFedCmHandler(storage Storage, tmpl *template.Template) *AddIdentityFedCmHandler {
	mux := http.NewServeMux()

	h := &AddIdentityFedCmHandler{
		mux: mux,
	}

	publicJwks, err := jwk.PublicSetOf(storage.GetJWKSet())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	prefix := storage.GetPrefix()
	loginKeyName := prefix + "login_key"

	mux.HandleFunc("/login-fedcm", func(w http.ResponseWriter, r *http.Request) {

		r.ParseForm()

		idents, _ := getIdentities(storage, r, publicJwks)

		data := struct {
			RootUri     string
			DisplayName string
			Identities  []*Identity
		}{
			RootUri:     storage.GetRootUri(),
			DisplayName: storage.GetDisplayName(),
			Identities:  idents,
		}

		returnUri := r.Form.Get("return_uri")
		fmt.Println("here", returnUri)
		setReturnUriCookie(storage, returnUri, w)

		err := tmpl.ExecuteTemplate(w, "login-fedcm.html", data)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}
	})
	mux.HandleFunc("/complete-login-fedcm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			io.WriteString(w, "Invalid method")
			return
		}

		r.ParseForm()

		// TODO: probably need to have a HTTP-only JWT cookie with a
		// PKCE code or something so we know this flow initiated from
		// us

		fedCmToken := r.Form.Get("fedcm-token")

		// Parse the token without verifying to get the issuer, then get the JWK for full validation

		unverifiedToken, err := jwt.ParseInsecure([]byte(fedCmToken), jwt.WithToken(openid.New()))
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		issuer := unverifiedToken.Issuer()

		ctx := context.Background()
		c := jwk.NewCache(ctx)

		oidcMeta, err := GetOidcConfiguration(issuer)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		err = c.Register(oidcMeta.JwksUri)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		keyset, err := c.Refresh(ctx, oidcMeta.JwksUri)
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		verifiedToken, err := jwt.Parse([]byte(fedCmToken), jwt.WithKeySet(keyset), jwt.WithToken(openid.New()))
		if err != nil {
			w.WriteHeader(500)
			io.WriteString(w, err.Error())
			return
		}

		oidcToken, ok := verifiedToken.(openid.Token)
		if !ok {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, "Not a valid OpenId Connect token")
			return
		}

		cookieValue := ""
		loginKeyCookie, err := r.Cookie(loginKeyName)
		if err == nil {
			cookieValue = loginKeyCookie.Value
		}

		email := oidcToken.Email()

		cookie, err := addIdentityToCookie(storage, issuer, email, email, cookieValue, true)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}

		returnUri, err := getReturnUriCookie(storage, r)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(os.Stderr, err.Error())
			return
		}
		deleteReturnUriCookie(storage, w)

		w.Header().Add("Set-Login", "logged-in")
		http.SetCookie(w, cookie)

		redirUrl := fmt.Sprintf("%s", returnUri)
		http.Redirect(w, r, redirUrl, http.StatusSeeOther)
	})

	return h
}

func (h *AddIdentityFedCmHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}
