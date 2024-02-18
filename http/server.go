package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/streadway/amqp"
)

type middleware func(http.Handler) http.Handler
type middlewares []middleware

type controller struct {
	logger        *log.Logger
	nextRequestID func() string
}

type Calculation struct {
	ID         int
	Expression string
	Status     string
	Answer     float64
}

type Agent struct {
	ID         int
	Last_Seen  string
	Status     string
	Goroutines int
}

var db *sql.DB

func (mws middlewares) apply(hdlr http.Handler) http.Handler {
	if len(mws) == 0 {
		return hdlr
	}
	return mws[1:].apply(mws[0](hdlr))
}

func (c *controller) shutdown(ctx context.Context, server *http.Server) context.Context {
	ctx, done := context.WithCancel(ctx)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer done()
		<-quit
		signal.Stop(quit)
		close(quit)
		server.ErrorLog.Printf("Server is shutting down...\n")
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			server.ErrorLog.Fatalf("Error while shutting down: %s\n", err)
		}
	}()
	return ctx
}

func (c *controller) logging(hdlr http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func(start time.Time) {
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" {
				requestID = "unknown"
			}
			c.logger.Println(requestID, req.Method, req.URL.Path, req.RemoteAddr, req.UserAgent(), time.Since(start))
		}(time.Now())
		hdlr.ServeHTTP(w, req)
	})
}

func (c *controller) tracing(hdlr http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := req.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = c.nextRequestID()
		}
		w.Header().Set("X-Request-Id", requestID)
		hdlr.ServeHTTP(w, req)
	})
}

func (c *controller) calculator(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/calculator" {
		http.NotFound(w, r)
		return
	}
	if r.Method == "POST" {
		expr := r.PostFormValue("text")
		cl := &Calculation{Expression: expr, Status: "in progress", Answer: 0}
		err := insertClIntoDB(cl)
		if err != nil {
			http.Error(w, "Internal Server Error", 500)
			return
		}
		cls, err := getClsFromDB()
		if err != nil {
			http.Error(w, "Internal Server Error", 500)
			return
		}
		cl.ID = cls[0].ID
		err = publishCl(cl)
		if err != nil {
			http.Error(w, "Internal Server Error", 500)
			return
		}
		calculations := map[string][]Calculation{
			"Calculations": cls,
		}
		tmpl := template.Must(template.ParseFiles("html/calculator.html"))
		tmpl.ExecuteTemplate(w, "calculations", calculations)
		return
	}
	cls, err := getClsFromDB()
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}
	calculations := map[string][]Calculation{
		"Calculations": cls,
	}
	tmpl := template.Must(template.ParseFiles("html/calculator.html"))
	tmpl.Execute(w, calculations)
}

func (c *controller) settings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings" {
		http.NotFound(w, r)
		return
	}
	if r.Method == "POST" {
		err := changeSettings(r)
		if err != nil {
			http.Error(w, "Internal Server Error", 500)
			return
		}
		agents, err := getAgentsFromDB()
		if err != nil {
			http.Error(w, "Internal Server Error", 500)
			return
		}
		ags := map[string][]Agent{
			"Agents": agents,
		}
		tmpl := template.Must(template.ParseFiles("html/settings.html"))
		tmpl.ExecuteTemplate(w, "agents", ags)
		return
	}
	arguments := make(map[string]any)
	err := getSettings(arguments)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}
	agents, err := getAgentsFromDB()
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}
	arguments["Agents"] = agents
	tmpl := template.Must(template.ParseFiles("html/settings.html"))
	tmpl.Execute(w, arguments)
}

func initDB() {
	for {
		var err error
		connStr := "postgres://postgres:pass@localhost:5432/go-pg?sslmode=disable"
		db, err = sql.Open("postgres", connStr)
		if err != nil {
			log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		if err = db.Ping(); err != nil {
			log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		log.Println("Connected to DB")
		break
	}
}

func getClsFromDB() ([]Calculation, error) {
	rowsRs, err := db.Query("SELECT * FROM Calculations")
	if err != nil {
		return nil, err
	}
	defer rowsRs.Close()
	cls := make([]Calculation, 0)
	for rowsRs.Next() {
		cl := Calculation{}
		err := rowsRs.Scan(&cl.ID, &cl.Expression, &cl.Status, &cl.Answer)
		if err != nil {
			return nil, err
		}
		cls = append(cls, cl)
	}
	slices.Reverse(cls)
	return cls, nil
}

func insertClIntoDB(c *Calculation) error {
	query := `INSERT INTO Calculations(expression, status, answer) VALUES($1, $2, $3)`
	_, err := db.Exec(query, c.Expression, c.Status, c.Answer)
	if err != nil {
		return err
	}
	return nil
}

func getAgentsFromDB() ([]Agent, error) {
	rowsRs, err := db.Query("SELECT * FROM Agents")
	if err != nil {
		return nil, err
	}
	defer rowsRs.Close()
	agents := make([]Agent, 0)
	for rowsRs.Next() {
		agent := Agent{}
		err := rowsRs.Scan(&agent.ID, &agent.Last_Seen, &agent.Status, &agent.Goroutines)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	slices.Reverse(agents)
	return agents, nil
}

func getSettings(arguments map[string]any) error {
	rowsRs, err := db.Query("SELECT * FROM Settings")
	if err != nil {
		return err
	}
	defer rowsRs.Close()
	for rowsRs.Next() {
		var name string
		var value int
		err := rowsRs.Scan(&name, &value)
		if err != nil {
			return err
		}
		arguments[name] = value
	}
	return nil
}

func changeSettings(r *http.Request) error {
	add_value := r.PostFormValue("add")
	sub_value := r.PostFormValue("sub")
	mult_value := r.PostFormValue("mult")
	div_value := r.PostFormValue("div")
	del_value := r.PostFormValue("del")
	query := `UPDATE Settings SET value = $1 WHERE name = 'add'`
	_, err := db.Exec(query, add_value)
	if err != nil {
		return err
	}
	query = `UPDATE Settings SET value = $1 WHERE name = 'sub'`
	_, err = db.Exec(query, sub_value)
	if err != nil {
		return err
	}
	query = `UPDATE Settings SET value = $1 WHERE name = 'mult'`
	_, err = db.Exec(query, mult_value)
	if err != nil {
		return err
	}
	query = `UPDATE Settings SET value = $1 WHERE name = 'div'`
	_, err = db.Exec(query, div_value)
	if err != nil {
		return err
	}
	query = `UPDATE Settings SET value = $1 WHERE name = 'del'`
	_, err = db.Exec(query, del_value)
	if err != nil {
		return err
	}
	return nil
}

func publishCl(c *Calculation) error {
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	q, err := ch.QueueDeclare(
		"calculations_queue",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	err = ch.Publish(
		"",
		q.Name,
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	)
	if err != nil {
		return err
	}
	return nil
}

func connectToRMQ() {
	for {
		conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
		if err != nil {
			log.Printf("An error occured while interacting with the broker, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		defer conn.Close()
		notify := conn.NotifyClose(make(chan *amqp.Error))
		ch, err := conn.Channel()
		if err != nil {
			log.Printf("An error occured while interacting with the broker, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		defer ch.Close()
		q, err := ch.QueueDeclare(
			"heartbeats_queue",
			false,
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			log.Fatalln(err)
		}
		deliveryChan, err := ch.Consume(
			q.Name,
			"",
			false,
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			log.Printf("An error occured while interacting with the broker, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		log.Println("Connected to RabbitMQ instance")
	receiving:
		for {
			select {
			case err = <-notify:
				log.Printf("An error occured while interacting with the broker, retrying in 30 seconds: %s", err)
				time.Sleep(30 * time.Second)
				break receiving
			case delivery := <-deliveryChan:
				id, err := strconv.Atoi(string(delivery.Body))
				if err != nil {
					log.Println("Invalid id format received from a heartbeat")
					continue
				}
				query := `UPDATE Agents SET last_seen = $1, status = $2 WHERE id = $3`
				_, err = db.Exec(query, time.Now().Local().Format("01/02/2006 15:04:05"), "active", id)
				if err != nil {
					log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
					time.Sleep(30 * time.Second)
					continue
				}
				log.Printf("Received a heartbeat from agent %s", delivery.Body)
				delivery.Ack(false)
			default:
				agents, err := getAgentsFromDB()
				if err != nil {
					log.Printf("An error occured while interacting with db, retrying in 30 seconds: %s", err)
					time.Sleep(30 * time.Second)
					continue
				}
				for _, agent := range agents {
					last_seen, err := time.ParseInLocation("01/02/2006 15:04:05", agent.Last_Seen, time.Local)
					if err != nil {
						log.Println("An error occured while parsing agent, deleting invalid item")
						query := `DELETE FROM Agents WHERE id = $1`
						_, err = db.Exec(query, agent.ID)
						if err != nil {
							log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
							time.Sleep(30 * time.Second)
							continue
						}
						continue
					}
					if time.Since(last_seen) >= 30*time.Second && agent.Status == "active" {
						log.Printf("No heartbeats received from agent %d for a long time, changing status to inactive", agent.ID)
						query := `UPDATE Agents SET status = $1 WHERE id = $2`
						_, err = db.Exec(query, "inactive", agent.ID)
						if err != nil {
							log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
							time.Sleep(1 * time.Minute)
							continue
						}
					} else if time.Since(last_seen) >= 1*time.Minute && agent.Status == "inactive" {
						log.Printf("No heartbeats received from agent %d for a very long time, changing status to dead", agent.ID)
						query := `UPDATE Agents SET status = $1 WHERE id = $2`
						_, err = db.Exec(query, "dead", agent.ID)
						if err != nil {
							log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
							time.Sleep(1 * time.Minute)
							continue
						}
					} else if agent.Status == "dead" {
						rowsRs, err := db.Query("SELECT value FROM Settings WHERE name = 'del'")
						if err != nil {
							log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
							time.Sleep(1 * time.Minute)
							continue
						}
						defer rowsRs.Close()
						var value int
						for rowsRs.Next() {
							err = rowsRs.Scan(&value)
						}
						if err != nil {
							log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
							time.Sleep(1 * time.Minute)
							break
						}
						if time.Since(last_seen) >= time.Duration(value)*time.Second {
							query := `DELETE FROM Agents WHERE id = $1`
							log.Println("Deleting dead agent")
							_, err = db.Exec(query, agent.ID)
							if err != nil {
								log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
								time.Sleep(30 * time.Second)
								continue
							}
						}
					}
				}
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func main() {
	port := 8080
	http_logger := log.New(os.Stdout, "http: ", log.LstdFlags)
	http_logger.Println("Server is starting...")
	c := &controller{logger: http_logger, nextRequestID: func() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }}
	router := http.NewServeMux()
	router.HandleFunc("/calculator", c.calculator)
	router.HandleFunc("/settings", c.settings)
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      (middlewares{c.tracing, c.logging}).apply(router),
		ErrorLog:     http_logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}
	ctx := c.shutdown(context.Background(), server)
	http_logger.Printf("Server is running at %d\n", port)
	initDB()
	defer db.Close()
	go connectToRMQ()
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		http_logger.Fatalf("Error on %d: %s\n", port, err)
	}
	<-ctx.Done()
	log.Println("Disconnected from DB")
	http_logger.Println("Server stopped")
}
