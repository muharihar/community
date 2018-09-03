// Copyright 2016 Documize Inc. <legal@documize.com>. All rights reserved.
//
// This software (Documize Community Edition) is licensed under
// GNU AGPL v3 http://www.gnu.org/licenses/agpl-3.0.en.html
//
// You can operate outside the AGPL restrictions by purchasing
// Documize Enterprise Edition and obtaining a commercial license
// by contacting <sales@documize.com>.
//
// https://documize.com

package ldap

import (
	"crypto/tls"
	// "database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	// "sort"
	// "strings"

	"github.com/documize/community/core/env"
	"github.com/documize/community/core/response"
	// "github.com/documize/community/core/secrets"
	"github.com/documize/community/core/streamutil"
	// "github.com/documize/community/core/stringutil"
	"github.com/documize/community/domain"
	// "github.com/documize/community/domain/auth"
	// usr "github.com/documize/community/domain/user"
	ath "github.com/documize/community/model/auth"
	lm "github.com/documize/community/model/auth"
	"github.com/documize/community/model/user"
	ld "gopkg.in/ldap.v2"
)

// Handler contains the runtime information such as logging and database.
type Handler struct {
	Runtime *env.Runtime
	Store   *domain.Store
}

// Preview connects to LDAP using paylaod and returns
// first 100 users for.
// and marks Keycloak disabled users as inactive.
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	h.Runtime.Log.Info("Sync'ing with LDAP")

	ctx := domain.GetRequestContext(r)
	if !ctx.Administrator {
		response.WriteForbiddenError(w)
		return
	}

	var result struct {
		Message string      `json:"message"`
		IsError bool        `json:"isError"`
		Users   []user.User `json:"users"`
	}

	// Read the request.
	defer streamutil.Close(r.Body)
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		result.Message = "Error: unable read request body"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Decode LDAP config.
	c := lm.LDAPConfig{}
	err = json.Unmarshal(body, &c)
	if err != nil {
		result.Message = "Error: unable read LDAP configuration payload"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	h.Runtime.Log.Info("Fetching LDAP users")

	users, err := fetchUsers(c)
	if err != nil {
		result.Message = "Error: unable fetch users from LDAP"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	result.IsError = false
	result.Message = fmt.Sprintf("Sync'ed with LDAP, found %d users", len(users))
	if len(users) > 100 {
		result.Users = users[:100]
	} else {
		result.Users = users
	}

	h.Runtime.Log.Info(result.Message)

	response.WriteJSON(w, result)
}

// Sync gets list of Keycloak users and inserts new users into Documize
// and marks Keycloak disabled users as inactive.
func (h *Handler) Sync(w http.ResponseWriter, r *http.Request) {
	ctx := domain.GetRequestContext(r)

	if !ctx.Administrator {
		response.WriteForbiddenError(w)
		return
	}

	var result struct {
		Message string `json:"message"`
		IsError bool   `json:"isError"`
	}

	// Org contains raw auth provider config
	org, err := h.Store.Organization.GetOrganization(ctx, ctx.OrgID)
	if err != nil {
		result.Message = "Error: unable to get organization record"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Exit if not using LDAP
	if org.AuthProvider != ath.AuthProviderLDAP {
		// 	result.Message = "Error: skipping user sync with LDAP as it is not the configured option"
		// 	result.IsError = true
		// 	response.WriteJSON(w, result)
		// 	h.Runtime.Log.Info(result.Message)
		// 	return
	}

	// Make Keycloak auth provider config
	c := lm.LDAPConfig{}
	// err = json.Unmarshal([]byte(org.AuthConfig), &c)
	// if err != nil {
	// 	result.Message = "Error: unable read LDAP configuration data"
	// 	result.IsError = true
	// 	response.WriteJSON(w, result)
	// 	h.Runtime.Log.Error(result.Message, err)
	// 	return
	// }

	c.ServerHost = "ldap.forumsys.com"
	c.ServerPort = 389
	c.EncryptionType = "none"
	c.BaseDN = "dc=example,dc=com"
	c.BindDN = "cn=read-only-admin,dc=example,dc=com"
	c.BindPassword = "password"
	c.UserFilter = ""
	c.GroupFilter = ""
	c.DisableLogout = false
	c.DefaultPermissionAddSpace = false

	address := fmt.Sprintf("%s:%d", c.ServerHost, c.ServerPort)

	h.Runtime.Log.Info("Connecting to LDAP server")

	l, err := ld.Dial("tcp", address)
	if err != nil {
		result.Message = "Error: unable to dial LDAP server: " + err.Error()
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}
	defer l.Close()

	if c.EncryptionType == "starttls" {
		h.Runtime.Log.Info("Using StartTLS with LDAP server")
		err = l.StartTLS(&tls.Config{InsecureSkipVerify: true})
		if err != nil {
			result.Message = "Error: unable to startTLS with LDAP server: " + err.Error()
			result.IsError = true
			response.WriteJSON(w, result)
			h.Runtime.Log.Error(result.Message, err)
			return
		}
	}

	// Authenticate with LDAP server using admin credentials.
	h.Runtime.Log.Info("Binding LDAP admin user")
	err = l.Bind(c.BindDN, c.BindPassword)
	if err != nil {
		result.Message = "Error: unable to bind specified admin user to LDAP: " + err.Error()
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Get users from LDAP server by using filter
	filter := ""
	attrs := []string{}
	if len(c.GroupFilter) > 0 {
		filter = fmt.Sprintf("(&(objectClass=group)(cn=%s))", c.GroupFilter)
		attrs = []string{"cn"}
	} else {
		filter = "(|(objectClass=person)(objectClass=user)(objectClass=inetOrgPerson))"
		attrs = []string{"dn", "cn", "givenName", "sn", "mail", "uid"}
	}

	searchRequest := ld.NewSearchRequest(
		c.BaseDN,
		ld.ScopeWholeSubtree, ld.NeverDerefAliases, 0, 0, false,
		filter,
		attrs,
		nil,
	)

	sr, err := l.Search(searchRequest)
	if err != nil {
		result.Message = "Error: unable to bind specified admin user to LDAP: " + err.Error()
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}
	fmt.Printf("entries found: %d", len(sr.Entries))

	for _, entry := range sr.Entries {
		fmt.Printf("[%s] %s (%s %s) @ %s\n",
			entry.GetAttributeValue("uid"),
			entry.GetAttributeValue("cn"),
			entry.GetAttributeValue("givenName"),
			entry.GetAttributeValue("sn"),
			entry.GetAttributeValue("mail"))
	}
	// // User list from LDAP
	// kcUsers, err := Fetch(c)
	// if err != nil {
	// 	result.Message = "Error: unable to fetch Keycloak users: " + err.Error()
	// 	result.IsError = true
	// 	response.WriteJSON(w, result)
	// 	h.Runtime.Log.Error(result.Message, err)
	// 	return
	// }

	// // User list from Documize
	// dmzUsers, err := h.Store.User.GetUsersForOrganization(ctx, "", 99999)
	// if err != nil {
	// 	result.Message = "Error: unable to fetch Documize users"
	// 	result.IsError = true
	// 	response.WriteJSON(w, result)
	// 	h.Runtime.Log.Error(result.Message, err)
	// 	return
	// }

	// sort.Slice(kcUsers, func(i, j int) bool { return kcUsers[i].Email < kcUsers[j].Email })
	// sort.Slice(dmzUsers, func(i, j int) bool { return dmzUsers[i].Email < dmzUsers[j].Email })

	// insert := []user.User{}

	// for _, k := range kcUsers {
	// 	exists := false

	// 	for _, d := range dmzUsers {
	// 		if k.Email == d.Email {
	// 			exists = true
	// 		}
	// 	}

	// 	if !exists {
	// 		insert = append(insert, k)
	// 	}
	// }

	// // Track the number of Keycloak users with missing data.
	// missing := 0

	// // Insert new users into Documize
	// for _, u := range insert {
	// 	if len(u.Email) == 0 {
	// 		missing++
	// 	} else {
	// 		err = addUser(ctx, h.Runtime, h.Store, u, c.DefaultPermissionAddSpace)
	// 	}
	// }

	// result.Message = fmt.Sprintf("LDAP sync found %d users, %d new users added, %d users with missing data ignored",
	// 	len(kcUsers), len(insert), missing)

	result.IsError = false
	result.Message = "Sync complete with LDAP server"

	response.WriteJSON(w, result)
	h.Runtime.Log.Info(result.Message)
}

// Authenticate checks Keycloak authentication credentials.
func (h *Handler) Authenticate(w http.ResponseWriter, r *http.Request) {
	// 	method := "authenticate"
	// 	ctx := domain.GetRequestContext(r)

	// 	defer streamutil.Close(r.Body)
	// 	body, err := ioutil.ReadAll(r.Body)
	// 	if err != nil {
	// 		response.WriteBadRequestError(w, method, "Bad payload")
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	a := ath.KeycloakAuthRequest{}
	// 	err = json.Unmarshal(body, &a)
	// 	if err != nil {
	// 		response.WriteBadRequestError(w, method, err.Error())
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	a.Domain = strings.TrimSpace(strings.ToLower(a.Domain))
	// 	a.Domain = h.Store.Organization.CheckDomain(ctx, a.Domain) // TODO optimize by removing this once js allows empty domains
	// 	a.Email = strings.TrimSpace(strings.ToLower(a.Email))

	// 	// Check for required fields.
	// 	if len(a.Email) == 0 {
	// 		response.WriteUnauthorizedError(w)
	// 		return
	// 	}

	// 	org, err := h.Store.Organization.GetOrganizationByDomain(a.Domain)
	// 	if err != nil {
	// 		response.WriteUnauthorizedError(w)
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	ctx.OrgID = org.RefID

	// 	// Fetch Keycloak auth provider config
	// 	ac := ath.KeycloakConfig{}
	// 	err = json.Unmarshal([]byte(org.AuthConfig), &ac)
	// 	if err != nil {
	// 		response.WriteBadRequestError(w, method, "Unable to unmarshall Keycloak Public Key")
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	// Decode and prepare RSA Public Key used by keycloak to sign JWT.
	// 	pkb, err := secrets.DecodeBase64([]byte(ac.PublicKey))
	// 	if err != nil {
	// 		response.WriteBadRequestError(w, method, "Unable to base64 decode Keycloak Public Key")
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}
	// 	pk := string(pkb)
	// 	pk = fmt.Sprintf("-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----", pk)

	// 	// Decode and verify Keycloak JWT
	// 	claims, err := auth.DecodeKeycloakJWT(a.Token, pk)
	// 	if err != nil {
	// 		response.WriteBadRequestError(w, method, err.Error())
	// 		h.Runtime.Log.Info("decodeKeycloakJWT failed")
	// 		return
	// 	}

	// 	// Compare the contents from JWT with what we have.
	// 	// Guards against MITM token tampering.
	// 	if a.Email != claims["email"].(string) {
	// 		response.WriteUnauthorizedError(w)
	// 		h.Runtime.Log.Info(">> Start Keycloak debug")
	// 		h.Runtime.Log.Info(a.Email)
	// 		h.Runtime.Log.Info(claims["email"].(string))
	// 		h.Runtime.Log.Info(">> End Keycloak debug")
	// 		return
	// 	}

	// 	h.Runtime.Log.Info("keycloak logon attempt " + a.Email + " @ " + a.Domain)

	// 	u, err := h.Store.User.GetByDomain(ctx, a.Domain, a.Email)
	// 	if err != nil && err != sql.ErrNoRows {
	// 		response.WriteServerError(w, method, err)
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	// Create user account if not found
	// 	if err == sql.ErrNoRows {
	// 		h.Runtime.Log.Info("keycloak add user " + a.Email + " @ " + a.Domain)

	// 		u = user.User{}
	// 		u.Firstname = a.Firstname
	// 		u.Lastname = a.Lastname
	// 		u.Email = a.Email
	// 		u.Initials = stringutil.MakeInitials(u.Firstname, u.Lastname)
	// 		u.Salt = secrets.GenerateSalt()
	// 		u.Password = secrets.GeneratePassword(secrets.GenerateRandomPassword(), u.Salt)

	// 		err = addUser(ctx, h.Runtime, h.Store, u, ac.DefaultPermissionAddSpace)
	// 		if err != nil {
	// 			response.WriteServerError(w, method, err)
	// 			h.Runtime.Log.Error(method, err)
	// 			return
	// 		}
	// 	}

	// 	// Password correct and active user
	// 	if a.Email != strings.TrimSpace(strings.ToLower(u.Email)) {
	// 		response.WriteUnauthorizedError(w)
	// 		return
	// 	}

	// 	// Attach user accounts and work out permissions.
	// 	usr.AttachUserAccounts(ctx, *h.Store, org.RefID, &u)

	// 	// No accounts signals data integrity problem
	// 	// so we reject login request.
	// 	if len(u.Accounts) == 0 {
	// 		response.WriteUnauthorizedError(w)
	// 		h.Runtime.Log.Error(method, err)
	// 		return
	// 	}

	// 	// Abort login request if account is disabled.
	// 	for _, ac := range u.Accounts {
	// 		if ac.OrgID == org.RefID {
	// 			if ac.Active == false {
	// 				response.WriteUnauthorizedError(w)
	// 				h.Runtime.Log.Error(method, err)
	// 				return
	// 			}
	// 			break
	// 		}
	// 	}

	// 	// Generate JWT token
	// 	authModel := ath.AuthenticationModel{}
	// 	authModel.Token = auth.GenerateJWT(h.Runtime, u.RefID, org.RefID, a.Domain)
	// 	authModel.User = u

	// 	response.WriteJSON(w, authModel)
}
