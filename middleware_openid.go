package main

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/context"

	"github.com/TykTechnologies/logrus"
	"github.com/TykTechnologies/openid2go/openid"
	"github.com/TykTechnologies/tyk/apidef"
)

var OIDPREFIX = "openid"

type OpenIDMW struct {
	*TykMiddleware
	providerConfiguration     *openid.Configuration
	provider_client_policymap map[string]map[string]string
	lock                      sync.RWMutex
}

func (k *OpenIDMW) GetName() string {
	return "OpenIDMW"
}

func (k *OpenIDMW) New() {
	k.provider_client_policymap = make(map[string]map[string]string)
	// Create an OpenID Configuration and store
	var err error
	k.providerConfiguration, err = openid.NewConfiguration(openid.ProvidersGetter(k.getProviders),
		openid.ErrorHandler(k.dummyErrorHandler))

	k.lock = sync.RWMutex{}

	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": OIDPREFIX,
		}).Error("OpenID configuration error: ", err)
	}
}

func (k *OpenIDMW) IsEnabledForSpec() bool { return true }

func (k *OpenIDMW) getProviders() ([]openid.Provider, error) {
	providers := []openid.Provider{}
	log.Debug("Setting up providers: ", k.TykMiddleware.Spec.OpenIDOptions.Providers)
	for _, provider := range k.TykMiddleware.Spec.OpenIDOptions.Providers {
		iss := provider.Issuer
		log.Debug("Setting up Issuer: ", iss)
		providerClientArray := make([]string, len(provider.ClientIDs))

		i := 0
		for clientID, policyID := range provider.ClientIDs {
			clID, _ := base64.StdEncoding.DecodeString(clientID)
			clientID := string(clID)

			k.lock.Lock()
			if k.provider_client_policymap[iss] == nil {
				k.provider_client_policymap[iss] = map[string]string{clientID: policyID}
			} else {
				k.provider_client_policymap[iss][clientID] = policyID
			}
			k.lock.Unlock()

			log.Debug("--> Setting up client: ", clientID, " with policy: ", policyID)
			providerClientArray[i] = clientID
			i++
		}

		p, err := openid.NewProvider(iss, providerClientArray)

		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix":   OIDPREFIX,
				"provider": iss,
			}).Error("Failed to create provider: ", err)
		} else {
			providers = append(providers, p)
		}
	}

	return providers, nil
}

// We don't want any of the error handling, we use our own
func (k *OpenIDMW) dummyErrorHandler(e error, w http.ResponseWriter, r *http.Request) bool {
	log.WithFields(logrus.Fields{
		"prefix": OIDPREFIX,
	}).Warning("JWT Invalid: ", e)
	return true
}

// GetConfig retrieves the configuration from the API config
func (k *OpenIDMW) GetConfig() (interface{}, error) {
	return nil, nil
}

func (k *OpenIDMW) ProcessRequest(w http.ResponseWriter, r *http.Request, configuration interface{}) (error, int) {
	// 1. Validate the JWT
	user, token, halt := openid.AuthenticateOIDWithUser(k.providerConfiguration, w, r)

	// 2. Generate the internal representation for the key
	if halt {
		// Fire Authfailed Event
		k.reportLoginFailure("[JWT]", r)
		return errors.New("Key not authorised"), 403
	}

	// 3. Create or set the session to match
	iss, found := token.Claims.(jwt.MapClaims)["iss"]
	clients, cfound := token.Claims.(jwt.MapClaims)["aud"]

	if !found && !cfound {
		log.WithFields(logrus.Fields{
			"prefix": OIDPREFIX,
		}).Error("No issuer or audiences found!")
		k.reportLoginFailure("[NOT GENERATED]", r)
		return errors.New("Key not authorised"), 403
	}

	k.lock.Lock()
	clientSet, foundIssuer := k.provider_client_policymap[iss.(string)]
	k.lock.Unlock()
	if !foundIssuer {
		log.WithFields(logrus.Fields{
			"prefix": OIDPREFIX,
		}).Error("No issuer or audiences found!")
		k.reportLoginFailure("[NOT GENERATED]", r)
		return errors.New("Key not authorised"), 403
	}

	policyID := ""
	clientID := ""
	switch v := clients.(type) {
	case string:
		k.lock.RLock()
		policyID = clientSet[v]
		k.lock.RUnlock()
		clientID = v
	case []interface{}:
		for _, audVal := range v {
			k.lock.RLock()
			policy, foundPolicy := clientSet[audVal.(string)]
			k.lock.RUnlock()
			if foundPolicy {
				clientID = audVal.(string)
				policyID = policy
				break
			}
		}
	}

	if policyID == "" {
		log.WithFields(logrus.Fields{
			"prefix": OIDPREFIX,
		}).Error("No matching policy found!")
		k.reportLoginFailure("[NOT GENERATED]", r)
		return errors.New("Key not authorised"), 403
	}

	data := []byte(user.ID)
	tokenID := fmt.Sprintf("%x", md5.Sum(data))
	sessionID := k.TykMiddleware.Spec.OrgID + tokenID
	if k.Spec.OpenIDOptions.SegregateByClient {
		// We are segregating by client, so use it as part of the internal token
		log.Debug("Client ID:", clientID)
		sessionID = k.TykMiddleware.Spec.OrgID + fmt.Sprintf("%x", md5.Sum([]byte(clientID))) + tokenID
	}

	log.Debug("Generated Session ID: ", sessionID)

	sessionState, exists := k.TykMiddleware.CheckSessionAndIdentityForValidKey(sessionID)
	if !exists {
		// Create it
		log.Debug("Key does not exist, creating")
		sessionState = SessionState{}

		// We need a base policy as a template, either get it from the token itself OR a proxy client ID within Tyk
		newSessionState, err := generateSessionFromPolicy(policyID,
			k.TykMiddleware.Spec.APIDefinition.OrgID,
			true)

		if err != nil {
			k.reportLoginFailure(sessionID, r)
			log.WithFields(logrus.Fields{
				"prefix": OIDPREFIX,
			}).Error("Could not find a valid policy to apply to this token!")
			return errors.New("Key not authorized: no matching policy"), 403
		}

		sessionState = newSessionState
		sessionState.MetaData = map[string]interface{}{"TykJWTSessionID": sessionID, "ClientID": clientID}
		sessionState.Alias = clientID + ":" + user.ID

		// Update the session in the session manager in case it gets called again
		k.Spec.SessionManager.UpdateSession(sessionID, sessionState, GetLifetime(k.Spec, &sessionState))
		log.Debug("Policy applied to key")

	}

	// 4. Set session state on context, we will need it later
	switch k.TykMiddleware.Spec.BaseIdentityProvidedBy {
	case apidef.OIDCUser, apidef.UnsetAuth:
		context.Set(r, SessionData, sessionState)
		context.Set(r, AuthHeaderValue, sessionID)
	}
	k.setContextVars(r, token)

	return nil, 200
}

func (k *OpenIDMW) reportLoginFailure(tykId string, r *http.Request) {
	log.WithFields(logrus.Fields{
		"prefix": OIDPREFIX,
		"key":    tykId,
	}).Warning("Attempted access with invalid key.")

	// Fire Authfailed Event
	AuthFailed(k.TykMiddleware, r, tykId)

	// Report in health check
	ReportHealthCheckValue(k.Spec.Health, KeyFailure, "1")
}

func (k *OpenIDMW) setContextVars(r *http.Request, token *jwt.Token) {
	// Flatten claims and add to context
	if k.Spec.EnableContextVars {
		cnt, contextFound := context.GetOk(r, ContextData)
		var contextDataObject map[string]interface{}
		if contextFound {
			contextDataObject = cnt.(map[string]interface{})
			claimPrefix := "jwt_claims_"

			for claimName, claimValue := range token.Claims.(jwt.MapClaims) {
				claim := claimPrefix + claimName
				contextDataObject[claim] = claimValue
			}

			// Key data
			authHeaderValue := context.Get(r, AuthHeaderValue)
			contextDataObject["token"] = authHeaderValue

			context.Set(r, ContextData, contextDataObject)
		}

	}
}
