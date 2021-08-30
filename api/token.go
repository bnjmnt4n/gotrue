package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jwt "github.com/golang-jwt/jwt"
	"github.com/netlify/gotrue/conf"
	"github.com/netlify/gotrue/metering"
	"github.com/netlify/gotrue/models"
	"github.com/netlify/gotrue/storage"
	"github.com/pkg/errors"
	"github.com/sethvargo/go-password/password"
)

// GoTrueClaims is a struct thats used for JWT claims
type GoTrueClaims struct {
	jwt.StandardClaims
	Email        string                 `json:"email"`
	Phone        string                 `json:"phone"`
	AppMetaData  map[string]interface{} `json:"app_metadata"`
	UserMetaData map[string]interface{} `json:"user_metadata"`
	Role         string                 `json:"role"`
}

// AccessTokenResponse represents an OAuth2 success response
type AccessTokenResponse struct {
	Token        string       `json:"access_token"`
	TokenType    string       `json:"token_type"` // Bearer
	ExpiresIn    int          `json:"expires_in"`
	RefreshToken string       `json:"refresh_token"`
	User         *models.User `json:"user"`
}

// PasswordGrantParams are the parameters the ResourceOwnerPasswordGrant method accepts
type PasswordGrantParams struct {
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

// RefreshTokenGrantParams are the parameters the RefreshTokenGrant method accepts
type RefreshTokenGrantParams struct {
	RefreshToken string `json:"refresh_token"`
}

// IdTokenGrantParams are the parameters the IdTokenGrant method accepts
type IdTokenGrantParams struct {
	IdToken  string `json:"id_token"`
	Nonce    string `json:"nonce"`
	Provider string `json:"provider"`
}

const useCookieHeader = "x-use-cookie"
const useSessionCookie = "session"

func (p *IdTokenGrantParams) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	config := getConfig(ctx)
	var provider *oidc.Provider
	var err error
	var clientId string
	switch p.Provider {
	case "apple":
		clientId = config.External.Apple.ClientID
		provider, err = oidc.NewProvider(ctx, "https://appleid.apple.com")
	case "azure":
		clientId = config.External.Azure.ClientID
		provider, err = oidc.NewProvider(ctx, "https://login.microsoftonline.com/common/v2.0")
	case "facebook":
		clientId = config.External.Facebook.ClientID
		provider, err = oidc.NewProvider(ctx, "https://www.facebook.com")
	case "google":
		clientId = config.External.Google.ClientID
		provider, err = oidc.NewProvider(ctx, "https://accounts.google.com")
	default:
		return nil, fmt.Errorf("Provider %s could not be found", p.Provider)
	}

	if err != nil {
		return nil, err
	}

	return provider.Verifier(&oidc.Config{ClientID: clientId}), nil
}

// Token is the endpoint for OAuth access token requests
func (a *API) Token(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	grantType := r.FormValue("grant_type")

	switch grantType {
	case "password":
		return a.ResourceOwnerPasswordGrant(ctx, w, r)
	case "refresh_token":
		return a.RefreshTokenGrant(ctx, w, r)
	case "id_token":
		return a.IdTokenGrant(ctx, w, r)
	default:
		return oauthError("unsupported_grant_type", "")
	}
}

// ResourceOwnerPasswordGrant implements the password grant type flow
func (a *API) ResourceOwnerPasswordGrant(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	params := &PasswordGrantParams{}

	jsonDecoder := json.NewDecoder(r.Body)
	if err := jsonDecoder.Decode(params); err != nil {
		return badRequestError("Could not read password grant params: %v", err)
	}

	cookie := r.Header.Get(useCookieHeader)

	aud := a.requestAud(ctx, r)
	instanceID := getInstanceID(ctx)
	config := a.getConfig(ctx)

	if params.Email != "" && params.Phone != "" {
		return unprocessableEntityError("Only an email address or phone number should be provided on login.")
	}
	var user *models.User
	var err error
	if params.Email != "" {
		user, err = models.FindUserByEmailAndAudience(a.db, instanceID, params.Email, aud)
	} else if params.Phone != "" {
		params.Phone = a.formatPhoneNumber(params.Phone)
		user, err = models.FindUserByPhoneAndAudience(a.db, instanceID, params.Phone, aud)
	} else {
		return oauthError("invalid_grant", "Invalid login credentials")
	}

	if err != nil {
		if models.IsNotFoundError(err) {
			return oauthError("invalid_grant", "Invalid login credentials")
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	if params.Email != "" && !user.IsConfirmed() {
		return oauthError("invalid_grant", "Email not confirmed")
	} else if params.Phone != "" && !user.IsPhoneConfirmed() {
		return oauthError("invalid_grant", "Phone not confirmed")
	}

	if !user.Authenticate(params.Password) {
		return oauthError("invalid_grant", "Invalid email or password")
	}

	var token *AccessTokenResponse
	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error
		if terr = models.NewAuditLogEntry(tx, instanceID, user, models.LoginAction, nil); terr != nil {
			return terr
		}
		if terr = triggerEventHooks(ctx, tx, LoginEvent, user, instanceID, config); terr != nil {
			return terr
		}

		token, terr = a.issueRefreshToken(ctx, tx, user)
		if terr != nil {
			return terr
		}

		if cookie != "" && config.Cookie.Duration > 0 {
			if terr = a.setCookieToken(config, token.Token, cookie == useSessionCookie, w); terr != nil {
				return internalServerError("Failed to set JWT cookie. %s", terr)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	metering.RecordLogin("password", user.ID, instanceID)
	token.User = user
	return sendJSON(w, http.StatusOK, token)
}

// RefreshTokenGrant implements the refresh_token grant type flow
func (a *API) RefreshTokenGrant(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	config := a.getConfig(ctx)
	instanceID := getInstanceID(ctx)

	params := &RefreshTokenGrantParams{}

	jsonDecoder := json.NewDecoder(r.Body)
	if err := jsonDecoder.Decode(params); err != nil {
		return badRequestError("Could not read refresh token grant params: %v", err)
	}

	cookie := r.Header.Get(useCookieHeader)

	if params.RefreshToken == "" {
		return oauthError("invalid_request", "refresh_token required")
	}

	user, token, err := models.FindUserWithRefreshToken(a.db, params.RefreshToken)
	if err != nil {
		if models.IsNotFoundError(err) {
			return oauthError("invalid_grant", "Invalid Refresh Token")
		}
		return internalServerError(err.Error())
	}

	if token.Revoked {
		a.clearCookieToken(ctx, w)
		return oauthError("invalid_grant", "Invalid Refresh Token").WithInternalMessage("Possible abuse attempt: %v", r)
	}

	var tokenString string
	var newToken *models.RefreshToken

	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error
		if terr = models.NewAuditLogEntry(tx, instanceID, user, models.TokenRefreshedAction, nil); terr != nil {
			return terr
		}

		newToken, terr = models.GrantRefreshTokenSwap(tx, user, token)
		if terr != nil {
			return internalServerError(terr.Error())
		}

		tokenString, terr = generateAccessToken(user, time.Second*time.Duration(config.JWT.Exp), config.JWT.Secret)
		if terr != nil {
			return internalServerError("error generating jwt token").WithInternalError(terr)
		}

		if cookie != "" && config.Cookie.Duration > 0 {
			if terr = a.setCookieToken(config, tokenString, cookie == useSessionCookie, w); terr != nil {
				return internalServerError("Failed to set JWT cookie. %s", terr)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	metering.RecordLogin("token", user.ID, instanceID)
	return sendJSON(w, http.StatusOK, &AccessTokenResponse{
		Token:        tokenString,
		TokenType:    "bearer",
		ExpiresIn:    config.JWT.Exp,
		RefreshToken: newToken.Token,
		User:         user,
	})
}

// IdTokenGrant implements the id_token grant type flow
func (a *API) IdTokenGrant(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	config := a.getConfig(ctx)
	instanceID := getInstanceID(ctx)

	params := &IdTokenGrantParams{}

	jsonDecoder := json.NewDecoder(r.Body)
	if err := jsonDecoder.Decode(params); err != nil {
		return badRequestError("Could not read id token grant params: %v", err)
	}

	if params.IdToken == "" || params.Nonce == "" || params.Provider == "" {
		return oauthError("invalid request", "id_token, nonce and provider required")
	}

	verifier, err := params.getVerifier(ctx)
	if err != nil {
		return err
	}

	idToken, err := verifier.Verify(ctx, params.IdToken)
	if err != nil {
		return badRequestError("%v", err)
	}

	claims := make(map[string]interface{})
	if err := idToken.Claims(&claims); err != nil {
		return err
	}

	// verify nonce to mitigate replay attacks
	hashedNonce, ok := claims["nonce"]
	if !ok {
		return oauthError("invalid request", "missing nonce in id_token")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(params.Nonce)))
	if hash != hashedNonce.(string) {
		return oauthError("invalid nonce", "").WithInternalMessage("Possible abuse attempt: %v", r)
	}

	// check if user exists already
	email, ok := claims["email"].(string)
	if !ok {
		return errors.New("Unable to find email associated to provider")
	}

	aud := claims["aud"].(string)
	user, err := models.FindUserByEmailAndAudience(a.db, instanceID, email, aud)

	if err != nil && !models.IsNotFoundError(err) {
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	var token *AccessTokenResponse
	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error
		if user == nil {
			if config.DisableSignup {
				return forbiddenError("Signups not allowed for this instance")
			}
			password, err := password.Generate(64, 10, 0, false, true)
			if err != nil {
				return internalServerError("error creating user").WithInternalError(err)
			}

			signupParams := &SignupParams{
				Provider: params.Provider,
				Email:    email,
				Password: password,
				Aud:      aud,
				Data:     claims,
			}

			user, terr = a.signupNewUser(ctx, tx, signupParams)
			if terr != nil {
				return terr
			}
		}

		if !user.IsConfirmed() {
			isEmailVerified := false
			emailVerified, ok := claims["email_verified"].(string)
			if ok {
				isEmailVerified, terr = strconv.ParseBool(emailVerified)
				if terr != nil {
					return terr
				}
			}
			if (!ok || !isEmailVerified) && !config.Mailer.Autoconfirm {
				mailer := a.Mailer(ctx)
				referrer := a.getReferrer(r)
				if terr = sendConfirmation(tx, user, mailer, config.SMTP.MaxFrequency, referrer); terr != nil {
					return internalServerError("Error sending confirmation mail").WithInternalError(terr)
				}
				return unauthorizedError("Error unverified email")
			}

			if terr := models.NewAuditLogEntry(tx, instanceID, user, models.UserSignedUpAction, nil); terr != nil {
				return terr
			}

			if terr = triggerEventHooks(ctx, tx, SignupEvent, user, instanceID, config); terr != nil {
				return terr
			}

			if terr = user.Confirm(tx); terr != nil {
				return internalServerError("Error updating user").WithInternalError(terr)
			}
		} else {
			if terr := models.NewAuditLogEntry(tx, instanceID, user, models.LoginAction, nil); terr != nil {
				return terr
			}
			if terr = triggerEventHooks(ctx, tx, LoginEvent, user, instanceID, config); terr != nil {
				return terr
			}
		}

		token, terr = a.issueRefreshToken(ctx, tx, user)
		if terr != nil {
			return oauthError("server_error", terr.Error())
		}
		return nil
	})

	if err != nil {
		return err
	}

	metering.RecordLogin("id_token", user.ID, instanceID)
	return sendJSON(w, http.StatusOK, &AccessTokenResponse{
		Token:        token.Token,
		TokenType:    token.TokenType,
		ExpiresIn:    token.ExpiresIn,
		RefreshToken: token.RefreshToken,
		User:         user,
	})
}

func generateAccessToken(user *models.User, expiresIn time.Duration, secret string) (string, error) {
	claims := &GoTrueClaims{
		StandardClaims: jwt.StandardClaims{
			Subject:   user.ID.String(),
			Audience:  user.Aud,
			ExpiresAt: time.Now().Add(expiresIn).Unix(),
		},
		Email:        user.GetEmail(),
		Phone:        user.GetPhone(),
		AppMetaData:  user.AppMetaData,
		UserMetaData: user.UserMetaData,
		Role:         user.Role,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

func (a *API) issueRefreshToken(ctx context.Context, conn *storage.Connection, user *models.User) (*AccessTokenResponse, error) {
	config := a.getConfig(ctx)

	now := time.Now()
	user.LastSignInAt = &now

	var tokenString string
	var refreshToken *models.RefreshToken

	err := conn.Transaction(func(tx *storage.Connection) error {
		var terr error
		refreshToken, terr = models.GrantAuthenticatedUser(tx, user)
		if terr != nil {
			return internalServerError("Database error granting user").WithInternalError(terr)
		}

		tokenString, terr = generateAccessToken(user, time.Second*time.Duration(config.JWT.Exp), config.JWT.Secret)
		if terr != nil {
			return internalServerError("error generating jwt token").WithInternalError(terr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &AccessTokenResponse{
		Token:        tokenString,
		TokenType:    "bearer",
		ExpiresIn:    config.JWT.Exp,
		RefreshToken: refreshToken.Token,
	}, nil
}

func (a *API) setCookieToken(config *conf.Configuration, tokenString string, session bool, w http.ResponseWriter) error {
	exp := time.Second * time.Duration(config.Cookie.Duration)
	cookie := &http.Cookie{
		Name:     config.Cookie.Key,
		Value:    tokenString,
		Secure:   true,
		HttpOnly: true,
		Path:     "/",
	}
	if !session {
		cookie.Expires = time.Now().Add(exp)
		cookie.MaxAge = config.Cookie.Duration
	}

	http.SetCookie(w, cookie)
	return nil
}

func (a *API) clearCookieToken(ctx context.Context, w http.ResponseWriter) {
	config := getConfig(ctx)
	http.SetCookie(w, &http.Cookie{
		Name:     config.Cookie.Key,
		Value:    "",
		Expires:  time.Now().Add(-1 * time.Hour * 10),
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		Path:     "/",
	})
}
