package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestLoginSuccessResponseRetainsJWTField(t *testing.T) {
	const secret = "compat-test-secret"
	cfg := config.Config{}
	cfg.Auth.AccessSecret = secret
	cfg.Auth.AccessExpire = 3600

	resp, err := NewLoginLogic(context.Background(), &svc.ServiceContext{Config: cfg}).Login(&types.LoginReq{SecretKey: secret})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var got struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			JWT string `json:"jwt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Code != 200 || got.Msg != "success" || got.Data.JWT == "" {
		t.Fatalf("login response lost compatibility fields: %s", encoded)
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(
		got.Data.JWT,
		claims,
		func(token *jwt.Token) (interface{}, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		t.Fatalf("returned JWT is not verifiable: token=%v err=%v", token, err)
	}
	exp, ok := claims["exp"].(float64)
	if !ok || time.Unix(int64(exp), 0).Before(time.Now()) {
		t.Fatalf("returned JWT has no valid expiration: %#v", claims)
	}
}
