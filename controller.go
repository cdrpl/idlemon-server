package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/crypto/bcrypt"
)

func CreateController(db *pgxpool.Pool, rdb *redis.Client, dc DataCache) Controller {
	return Controller{db: db, rdb: rdb, dc: dc}
}

type Controller struct {
	db  *pgxpool.Pool
	rdb *redis.Client
	dc  DataCache
}

/* App Routes */

func (c Controller) HealthCheck(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	JsonSuccess(w)
}

// Route will return the current server version and expected client version.
func (c Controller) Version(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	m := map[string]string{
		"server": os.Getenv("SERVER_VERSION"),
		"client": os.Getenv("CLIENT_VERSION"),
	}
	JsonRes(w, m)
}

func (c Controller) Robots(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	_, err := io.WriteString(w, robots)
	if err != nil {
		log.Fatalf("fail to write robots.txt to http response writer: %v\n", err)
	}
}

func (c Controller) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	ErrRes(w, http.StatusMethodNotAllowed)
}

func (c Controller) NotFound(w http.ResponseWriter, r *http.Request) {
	ErrRes(w, http.StatusNotFound)
}

/* Campaign Routes */

func (c Controller) CampaignCollect(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	userID := GetUserID(r)

	tx, err := c.db.Begin(r.Context())
	if err != nil {
		log.Printf("campaign collect error: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	user, err := FindUserLock(r.Context(), tx, userID)
	if err != nil {
		log.Printf("fail to find user: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	exp, gold, expStones := user.Data.Campaign.Collect(&user)

	if exp > 0 || gold > 0 || expStones > 0 {
		err := UpdateUserLock(r.Context(), tx, user)
		if err != nil {
			log.Printf("fail to update user data: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}

		if err := tx.Commit(r.Context()); err != nil {
			log.Printf("campaign collect error: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	res := CampaignCollectRes{
		Exp:             exp,
		Gold:            gold,
		ExpStones:       expStones,
		LastCollectedAt: user.Data.Campaign.LastCollectedAt,
	}

	log.Printf("user %v has collected resources: {exp:%v gold:%v expStones:%v}\n", userID, res.Exp, res.Gold, res.ExpStones)
	JsonRes(w, res)
}

/* Daily Quest Routes */

func (c Controller) DailyQuestComplete(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	userID := GetUserID(r)

	questID, err := strconv.Atoi(p.ByName("id"))
	if err != nil {
		log.Printf("daily quest complete, quest ID could not be converted to int: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	tx, err := c.db.Begin(r.Context())
	if err != nil {
		log.Printf("daily quest complete, fail to begin transaction: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	user, err := FindUserLock(r.Context(), tx, userID)
	if err != nil {
		log.Printf("daily quest complete, fail to fetch user_daily_quest row: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	dailyQuest := c.dc.DailyQuests[questID]
	userDailyQuest := &user.Data.DailyQuestProgress[questID]

	if userDailyQuest.IsCompleted() {
		JsonRes(w, DailyQuestCompleteRes{Status: 1, Message: "already completed"})
		return
	}

	if userDailyQuest.Count < dailyQuest.Required {
		JsonRes(w, DailyQuestCompleteRes{Status: 2, Message: "requirements not met"})
		return
	}

	userDailyQuest.Complete()
	dailyQuest.Reward.Apply(&user)

	err = UpdateUserLock(r.Context(), tx, user)
	if err != nil {
		log.Printf("fail to update user: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("daily quest complete, failed to commit transaction: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("user %v completed daily quest %v", user.ID, questID)
	res := DailyQuestCompleteRes{Status: 0, Reward: dailyQuest.Reward}
	JsonRes(w, res)
}

/* Unit Routes */

// Toggle a unit's lock. Only works on units owned by the user.
func (c Controller) UnitToggleLock(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	userID := GetUserID(r)
	unitID := p.ByName("id")

	tx, err := c.db.Begin(r.Context())
	if err != nil {
		log.Printf("fail to begin transaction: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	user, err := FindUserLock(r.Context(), tx, userID)
	if err != nil {
		log.Printf("fail to find user: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	if unit, ok := user.Data.Units[unitID]; ok {
		unit.IsLocked = !unit.IsLocked
		user.Data.Units[unitID] = unit

		err := UpdateUserLock(r.Context(), tx, user)
		if err != nil {
			log.Printf("fail to update user: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}

		if err := tx.Commit(r.Context()); err != nil {
			log.Printf("fail to commit transaction: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
			return
		}

		log.Printf("user %v change lock %v for unit %v", userID, !unit.IsLocked, unit.ID)
		JsonSuccess(w)
		return
	}

	ErrRes(w, http.StatusNotFound)
}

/* User Routes */

func (c Controller) SignUp(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	req := GetReqDTO(r).(*SignUpReq)

	exists, err := NameExists(r.Context(), c.db, req.Name)
	if err != nil {
		log.Printf("sign up error: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	} else if exists {
		ErrResCustom(w, http.StatusBadRequest, "name is already taken")
		return
	}

	exists, err = EmailExists(r.Context(), c.db, req.Email)
	if err != nil {
		log.Printf("sign up error: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	} else if exists {
		ErrResCustom(w, http.StatusBadRequest, "an account with this email already exists")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Pass), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("fail to hash user password: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	_, err = InsertUser(r.Context(), c.db, CreateUser(c.dc, req.Name, req.Email, string(hash)))
	if err != nil {
		log.Printf("sign up error: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("new user registration: {name:%v email:%v}\n", req.Name, req.Email)
	JsonSuccess(w)
}

func (c Controller) SignIn(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	signInReq := GetReqDTO(r).(*SignInReq)

	user := User{}
	query := "SELECT id, name, pass, created_at, data FROM users WHERE email = $1"
	err := c.db.QueryRow(r.Context(), query, signInReq.Email).Scan(&user.ID, &user.Name, &user.Pass, &user.CreatedAt, &user.Data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			ErrRes(w, http.StatusUnauthorized)
		} else {
			log.Printf("sign in error: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(user.Pass), []byte(signInReq.Pass))
	if err != nil {
		ErrRes(w, http.StatusUnauthorized)
		return
	}

	token, err := CreateApiToken(r.Context(), c.rdb, user.ID)
	if err != nil {
		log.Printf("sign in error: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	// increase daily sign in quest count
	user.Data.DailyQuestProgress[DAILY_QUEST_SIGN_IN].Count++

	// save updated user data
	err = UpdateUser(r.Context(), c.db, user)
	if err != nil {
		log.Printf("fail to update user data: %v\n", err)
		ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		return
	}

	user.Email = signInReq.Email
	user.Pass = ""

	signInRes := SignInRes{
		Token:         token,
		User:          user,
		UnitTemplates: c.dc.UnitTemplates,
	}

	log.Printf("user sign in: {id:%v name:%v email:%v}\n", user.ID, user.Name, user.Email)
	JsonRes(w, signInRes)
}

func (c Controller) UserRename(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	id := GetUserID(r)
	req := GetReqDTO(r).(*UserRenameReq)

	_, err := c.db.Exec(r.Context(), "UPDATE users SET name = $1 WHERE id = $2", req.Name, id)
	if err != nil {
		var pgErr *pgconn.PgError

		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			ErrResCustom(w, http.StatusBadRequest, "the name is already taken")
		} else {
			log.Printf("user rename error: %v\n", err)
			ErrResSanitize(w, http.StatusInternalServerError, err.Error())
		}

		return
	}

	log.Printf("user %v change name to %v\n", id, req.Name)

	res := map[string]string{"name": req.Name}
	JsonRes(w, res)
}
