// Copyright 2016 The go-daylight Authors
// This file is part of the go-daylight library.
//
// The go-daylight library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-daylight library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-daylight library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/GenesisKernel/go-genesis/packages/conf"
	"github.com/GenesisKernel/go-genesis/packages/consts"
	"github.com/GenesisKernel/go-genesis/packages/converter"
	"github.com/GenesisKernel/go-genesis/packages/crypto"
	"github.com/GenesisKernel/go-genesis/packages/model"
	"github.com/GenesisKernel/go-genesis/packages/notificator"
	"github.com/GenesisKernel/go-genesis/packages/publisher"
	jwt "github.com/dgrijalva/jwt-go"

	log "github.com/sirupsen/logrus"
)

// Special word used by frontend to sign UID generated by /getuid API command, sign is performed for contcatenated word and UID
const nonceSalt = "LOGIN"

type loginForm struct {
	form
	EcosystemID int64    `schema:"ecosystem"`
	Expire      int64    `schema:"expire"`
	PublicKey   hexValue `schema:"pubkey"`
	KeyID       int64    `schema:"key_id"`
	Signature   hexValue `schema:"signature"`
	RoleID      int64    `schema:"role_id"`
	IsMobile    bool     `schema:"mobile"`
}

type loginResult struct {
	Token       string       `json:"token,omitempty"`
	Refresh     string       `json:"refresh,omitempty"`
	EcosystemID string       `json:"ecosystem_id,omitempty"`
	KeyID       string       `json:"key_id,omitempty"`
	Address     string       `json:"address,omitempty"`
	NotifyKey   string       `json:"notify_key,omitempty"`
	IsNode      bool         `json:"isnode,omitempty"`
	IsOwner     bool         `json:"isowner,omitempty"`
	IsVDE       bool         `json:"vde,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Roles       []roleResult `json:"roles,omitempty"`
}

type roleResult struct {
	RoleId   int64  `json:"role_id"`
	RoleName string `json:"role_name"`
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var (
		uid       string
		publicKey []byte
		wallet    int64
		err       error
		form      = &loginForm{}
	)

	if err := parseForm(r, form); err != nil {
		errorResponse(w, err)
		return
	}

	if uid, err = getUID(r); err != nil {
		errorResponse(w, err)
		return
	}

	client := getClient(r)
	logger := getLogger(r)

	if form.EcosystemID > 0 {
		client.EcosystemID = form.EcosystemID
	} else if client.EcosystemID == 0 {
		logger.WithFields(log.Fields{"type": consts.EmptyObject}).Warning("state is empty, using 1 as a state")
		client.EcosystemID = 1
	}

	publicKey = form.PublicKey.Value()
	if len(publicKey) == 0 {
		logger.WithFields(log.Fields{"type": consts.EmptyObject}).Error("public key is empty")
		errorResponse(w, errEmptyPublic)
		return
	}
	wallet = crypto.Address(publicKey)

	account, err := getAccount(r, client.EcosystemID, wallet)
	if err != nil {
		errorResponse(w, err)
		return
	}

	if account == nil {
		contract := getContract(r, "NewUser")
		contract.CreateTx(hex.EncodeToString(publicKey))
	} else {
		publicKey = account.PublicKey
	}

	if client.RoleID == 0 && form.RoleID != 0 {
		checkedRole, err := checkRoleFromParam(form.RoleID, client.EcosystemID, wallet)
		if err != nil {
			errorResponse(w, err)
			return
		}

		if checkedRole != form.RoleID {
			errorResponse(w, errCheckRole)
			return
		}

		client.RoleID = checkedRole
	}

	verify, err := crypto.CheckSign(publicKey, nonceSalt+uid, form.Signature.Value())
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.CryptoError, "pubkey": publicKey, "uid": uid, "signature": form.Signature}).Error("checking signature")
		errorResponse(w, newError(err, http.StatusBadRequest))
		return
	}
	if !verify {
		logger.WithFields(log.Fields{"type": consts.InvalidObject, "pubkey": publicKey, "uid": uid, "signature": form.Signature}).Error("incorrect signature")
		errorResponse(w, errSignature)
		return
	}

	var founder int64
	if founder, err = getFounder(r, client.EcosystemID); err != nil {
		errorResponse(w, err)
		return
	}

	result := &loginResult{
		EcosystemID: converter.Int64ToStr(client.EcosystemID),
		KeyID:       converter.Int64ToStr(wallet),
		Address:     crypto.KeyToAddress(publicKey),
		IsOwner:     founder == wallet,
		IsNode:      conf.Config.KeyID == wallet,
		IsVDE:       model.IsTable(fmt.Sprintf(`%d_vde_tables`, client.EcosystemID)),
	}

	expire := form.Expire
	if expire == 0 {
		logger.WithFields(log.Fields{"type": consts.JWTError, "expire": jwtExpire}).Warning("using expire from jwt")
		expire = jwtExpire
	}

	claims := JWTClaims{
		KeyID:       result.KeyID,
		EcosystemID: result.EcosystemID,
		IsMobile:    strconv.FormatBool(form.IsMobile),
		RoleID:      converter.Int64ToStr(client.RoleID),
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: time.Now().Add(time.Second * time.Duration(expire)).Unix(),
		},
	}

	result.Token, err = generateJWTToken(claims)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.JWTError, "error": err}).Error("generating jwt token")
		errorResponse(w, err)
		return
	}
	claims.StandardClaims.ExpiresAt = time.Now().Add(time.Hour * 30 * 24).Unix()
	result.Refresh, err = generateJWTToken(claims)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.JWTError, "error": err}).Error("generating jwt token")
		errorResponse(w, err)
		return
	}
	result.NotifyKey, result.Timestamp, err = publisher.GetHMACSign(wallet)
	if err != nil {
		errorResponse(w, err)
		return
	}

	ra := &model.RolesParticipants{}
	roles, err := ra.SetTablePrefix(client.EcosystemID).GetActiveMemberRoles(wallet)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting roles")
		errorResponse(w, errServer)
		return
	}

	for _, r := range roles {
		var res map[string]string
		if err := json.Unmarshal([]byte(r.Role), &res); err != nil {
			logger.WithFields(log.Fields{"type": consts.JSONUnmarshallError, "error": err}).Error("unmarshalling role")
			errorResponse(w, errServer)
			return
		} else {
			result.Roles = append(result.Roles, roleResult{
				RoleId:   converter.StrToInt64(res["id"]),
				RoleName: res["name"],
			})
		}
	}
	notificator.AddUser(wallet, client.EcosystemID)
	notificator.UpdateNotifications(client.EcosystemID, []int64{wallet})

	jsonResponse(w, result)
}

func getUID(r *http.Request) (string, error) {
	var uid string

	token := getToken(r)
	if token != nil {
		if claims, ok := token.Claims.(*JWTClaims); ok {
			uid = claims.UID
		}
	} else if len(uid) == 0 {
		getLogger(r).WithFields(log.Fields{"type": consts.EmptyObject}).Error("UID is empty")
		return "", errUnknownUID
	}

	return uid, nil
}

func getAccount(r *http.Request, ecosystemID, keyID int64) (*model.Key, error) {
	account := &model.Key{}
	account.SetTablePrefix(ecosystemID)
	found, err := account.Get(keyID)
	if err != nil {
		logger := getLogger(r)
		logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("selecting record from keys")
		return nil, err
	} else if found {
		if account.Deleted == 1 {
			return nil, errDeletedKey
		}
	}
	return account, nil
}

func checkRoleFromParam(role, ecosystemID, wallet int64) (int64, error) {
	if role > 0 {
		ok, err := model.MemberHasRole(nil, ecosystemID, wallet, role)
		if err != nil {
			log.WithFields(log.Fields{
				"type":      consts.DBError,
				"member":    wallet,
				"role":      role,
				"ecosystem": ecosystemID}).Error("check role")

			return 0, err
		}

		if !ok {
			log.WithFields(log.Fields{
				"type":      consts.NotFound,
				"member":    wallet,
				"role":      role,
				"ecosystem": ecosystemID,
			}).Error("member hasn't role")

			return 0, nil
		}
	}
	return role, nil
}

func getFounder(r *http.Request, ecosystemID int64) (int64, error) {
	var (
		sp      model.StateParameter
		founder int64
	)

	sp.SetTablePrefix(converter.Int64ToStr(ecosystemID))
	if ok, err := sp.Get(nil, "founder_account"); err != nil {
		getLogger(r).WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting founder_account parameter")
		return founder, errServer
	} else if ok {
		founder = converter.StrToInt64(sp.Value)
	}

	return founder, nil
}
