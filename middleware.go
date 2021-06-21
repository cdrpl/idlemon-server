package main

import (
	"context"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
)

func CreateBodyParserMiddleware() BodyParserMiddleware {
	return BodyParserMiddleware{validate: validator.New()}
}

type BodyParserMiddleware struct {
	validate *validator.Validate
}

// Will only accept reflect types of request.DTO.
func (bpm BodyParserMiddleware) Middleware(dtotype reflect.Type, next httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			ErrResCustom(w, http.StatusInternalServerError, err.Error())
			return
		}

		if contentType != "application/json" {
			ErrResCustom(w, http.StatusUnsupportedMediaType, "content type header must be application/json")
			return
		}

		bytes, err := ReadReqBody(w, r)
		if err != nil {
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}

		dto := reflect.New(dtotype).Interface().(RequestDTO)
		err = dto.Deserialize(bytes)
		if err != nil {
			ErrResSanitize(w, http.StatusBadRequest, err.Error())
			return
		}

		msg, hasError := RunStructValidator(bpm.validate, dto)
		if hasError {
			ErrResCustom(w, http.StatusBadRequest, msg)
			return
		}

		ctx := context.WithValue(r.Context(), ReqDtoCtx, dto)
		next(w, r.WithContext(ctx), p)
	}
}

func CreateRequireTokenMiddleware(rdb *redis.Client) RequireTokenMiddleware {
	return RequireTokenMiddleware{rdb: rdb}
}

// This middleware will reject requests that don't have a valid authorization header.
type RequireTokenMiddleware struct {
	rdb *redis.Client
}

// Will make sure the authorization header has valid credentials.
// If authorization fails, an error response will be written.
func (rt RequireTokenMiddleware) Middleware(next httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		id, token := ParseAuthHeader(r.Header.Get("Authorization"))

		authorized, err := ValidateApiToken(r.Context(), id, token, rt.rdb)
		if err != nil {
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}

		if authorized {
			userId, err := uuid.Parse(id)
			if err != nil {
				ErrResSanitize(w, http.StatusBadRequest, err.Error())
			} else {
				ctx := context.WithValue(r.Context(), UserIdCtx, userId)
				next(w, r.WithContext(ctx), p)
			}
		} else {
			ErrRes(w, http.StatusUnauthorized)
		}
	}
}

// Extract the id and token split by a colon.
func ParseAuthHeader(authorization string) (id string, token string) {
	index := strings.Index(authorization, ":")

	if index != -1 {
		id = authorization[:index]
		token = authorization[index+1:]
	}

	return
}
