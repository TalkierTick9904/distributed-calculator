package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/streadway/amqp"
)

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

func parenthesesCheck(expression string) bool {
	var count int
	for _, char := range expression {
		if string(char) == "(" {
			count++
		} else if string(char) == ")" {
			count--
		}
		if count < 0 {
			return false
		}
	}
	if count != 0 {
		return false
	}
	return true
}

func parenthesesClear(expression string) string {
	patternOneNum := `\(\-??\d+(\.\d+)?\)`
	patternDups := `\({2,}.*?\){2,}`
	clearMap := make(map[int]string)
	processing := true
	for processing {
		processing = false
		re := regexp.MustCompile(patternOneNum)
		matches := re.FindAllStringSubmatchIndex(expression, -1)
		for _, match := range matches {
			startInd, endInd := match[0], match[1]
			simpleExpr := expression[startInd:endInd]
			clearMap[startInd] = simpleExpr
			processing = true
		}
		for k, v := range clearMap {
			expression = expression[:k] + expression[k+1:k+len(v)-1] + expression[k+len(v):]
			delete(clearMap, k)
		}
		re = regexp.MustCompile(patternDups)
		matches = re.FindAllStringSubmatchIndex(expression, -1)
		for _, match := range matches {
			startInd, endInd := match[0], match[1]
			simpleExpr := expression[startInd:endInd]
			clearMap[startInd] = simpleExpr
			processing = true
		}
		for k, v := range clearMap {
			expression = expression[:k] + expression[k+1:k+len(v)-1] + expression[k+len(v):]
			delete(clearMap, k)
		}
	}
	return expression
}

func extractSimpleExpressions(expression string) map[int]string {
	patternAddSub := `\d+(\.\d+)?[+\-]\-??\d+(\.\d+)?`
	patternMultDiv := `\d+(\.\d+)?[*/]\-??\d+(\.\d+)?`
	simpleExprMap := make(map[int]string)
	re := regexp.MustCompile(patternAddSub)
	matches := re.FindAllStringSubmatchIndex(expression, -1)
	for _, match := range matches {
		startInd, endInd := match[0], match[1]
		if startInd == 0 && endInd == len(expression) {
			simpleExpr := expression[startInd:endInd]
			simpleExprMap[startInd] = simpleExpr
		} else if startInd == 0 {
			if !strings.ContainsAny(string(expression[endInd]), "*/") {
				simpleExpr := expression[startInd:endInd]
				simpleExprMap[startInd] = simpleExpr
			}
		} else if endInd == len(expression) {
			if startInd == 1 {
				if string(expression[startInd-1]) == "-" {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			} else if startInd >= 2 {
				if !regexp.MustCompile(`\d`).MatchString(string(expression[startInd-2])) && string(expression[startInd-1]) == "-" {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			}
			if strings.ContainsAny(string(expression[startInd-1]), "+(") {
				simpleExpr := expression[startInd:endInd]
				simpleExprMap[startInd] = simpleExpr
			}
		} else {
			if startInd == 1 {
				if string(expression[startInd-1]) == "-" && !strings.ContainsAny(string(expression[endInd]), "*/") {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			} else if startInd >= 2 {
				if !regexp.MustCompile(`\d`).MatchString(string(expression[startInd-2])) && string(expression[startInd-1]) == "-" && !strings.ContainsAny(string(expression[endInd]), "*/") {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			}
			if strings.ContainsAny(string(expression[startInd-1]), "+(") && !strings.ContainsAny(string(expression[endInd]), "*/") {
				simpleExpr := expression[startInd:endInd]
				simpleExprMap[startInd] = simpleExpr
			}
		}
	}
	re = regexp.MustCompile(patternMultDiv)
	matches = re.FindAllStringSubmatchIndex(expression, -1)
	for _, match := range matches {
		startInd, endInd := match[0], match[1]
		if startInd == 0 {
			simpleExpr := expression[startInd:endInd]
			simpleExprMap[startInd] = simpleExpr
		} else {
			if startInd == 1 {
				if string(expression[startInd-1]) == "-" {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			} else if startInd >= 2 {
				if !regexp.MustCompile(`\d`).MatchString(string(expression[startInd-2])) && string(expression[startInd-1]) == "-" {
					simpleExpr := expression[startInd-1 : endInd]
					simpleExprMap[startInd-1] = simpleExpr
					continue
				}
			}
			if !strings.ContainsAny(string(expression[startInd-1]), "/") {
				simpleExpr := expression[startInd:endInd]
				simpleExprMap[startInd] = simpleExpr
			}
		}
	}
	return simpleExprMap
}

func evaluateSimpleExpression(expression string, operations map[string]int, answMap map[string]float64, errChan chan error, mu *sync.Mutex, wg *sync.WaitGroup) {
	defer wg.Done()
	re := regexp.MustCompile(`(\-??\d+(\.\d+)?)([+\-*/])(\-??\d+(\.\d+)?)`)
	res := re.FindStringSubmatch(expression)
	parts := []string{res[1], res[3], res[4]}
	operand1, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		errChan <- err
		return
	}
	operand2, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		errChan <- err
		return
	}
	var result float64
	switch parts[1] {
	case "+":
		result = operand1 + operand2
		time.Sleep(time.Duration(operations["add"]) * time.Second)
	case "-":
		result = operand1 - operand2
		time.Sleep(time.Duration(operations["sub"]) * time.Second)
	case "*":
		result = operand1 * operand2
		time.Sleep(time.Duration(operations["mult"]) * time.Second)
	case "/":
		if operand2 == 0 {
			errChan <- fmt.Errorf("division by zero")
			return
		}
		result = operand1 / operand2
		time.Sleep(time.Duration(operations["div"]) * time.Second)
	default:
		errChan <- fmt.Errorf("invalid operator: %s", parts[1])
		return
	}
	errChan <- nil
	mu.Lock()
	defer mu.Unlock()
	answMap[expression] = result
}

func evaluateComplexExpression(expression string, numGoroutines int, operations map[string]int) (float64, error) {
	expression = strings.ReplaceAll(expression, " ", "")
	isValid := regexp.MustCompile(`^\(*\-??\d+(\.\d+)?([+\-/*]\(*\-??\d+(\.\d+)?\)*)+\)*$`).MatchString(expression) && parenthesesCheck(expression)
	if !isValid {
		return 0, fmt.Errorf("not valid")
	}
	expression = parenthesesClear(expression)
	simpleExprMap := extractSimpleExpressions(expression)
	for len(simpleExprMap) != 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		errChan := make(chan error, numGoroutines)
		var count int
		answMap := make(map[string]float64)
		for _, v := range simpleExprMap {
			if count < numGoroutines {
				count++
				wg.Add(1)
				go evaluateSimpleExpression(v, operations, answMap, errChan, &mu, &wg)
			} else {
				count = 1
				wg.Wait()
				for i := 0; i < numGoroutines; i++ {
					select {
					case err := <-errChan:
						if err != nil {
							close(errChan)
							return 0, err
						}
					}
				}
				wg.Add(1)
				go evaluateSimpleExpression(v, operations, answMap, errChan, &mu, &wg)
			}
		}
		wg.Wait()
		for i := 0; i < count; i++ {
			select {
			case err := <-errChan:
				if err != nil {
					close(errChan)
					return 0, err
				}
			}
		}
		close(errChan)
		for k, v := range simpleExprMap {
			result := answMap[v]
			old := len(expression)
			expression = expression[:k] + strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", result), "0"), ".") + expression[k+len(v):]
			expression = parenthesesClear(expression)
			if old > len(expression) {
				diff := old - len(expression)
				for k1, v1 := range simpleExprMap {
					if k1 > k {
						delete(simpleExprMap, k1)
						simpleExprMap[k1-diff] = v1
					}
				}
			} else if old < len(expression) {
				diff := len(expression) - old
				for k1, v1 := range simpleExprMap {
					if k1 > k {
						delete(simpleExprMap, k1)
						simpleExprMap[k1+diff] = v1
					}
				}
			}
		}
		simpleExprMap = extractSimpleExpressions(expression)
	}
	return strconv.ParseFloat(expression, 64)
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

func insertAgentIntoDB(agent *Agent) error {
	query := `INSERT INTO Agents(last_seen, status, goroutines) VALUES($1, $2, $3)`
	_, err := db.Exec(query, agent.Last_Seen, agent.Status, agent.Goroutines)
	if err != nil {
		return err
	}
	return nil
}

func getSettings() (map[string]int, error) {
	rowsRs, err := db.Query("SELECT * FROM Settings")
	if err != nil {
		return nil, err
	}
	defer rowsRs.Close()
	values := make(map[string]int)
	for rowsRs.Next() {
		var name string
		var value int
		err := rowsRs.Scan(&name, &value)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, nil
}

func connectToRMQ(id int, numGoroutines int) {
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
			"calculations_queue",
			false,
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			log.Fatalln(err)
		}
		q2, err := ch.QueueDeclare(
			"heartbeats_queue",
			false,
			false,
			false,
			false,
			nil,
		)
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
		go func() {
			for {
				body := strconv.Itoa(id)
				err = ch.Publish(
					"",
					q2.Name,
					false,
					false,
					amqp.Publishing{
						ContentType: "text/plain",
						Body:        []byte(body),
					})
				if err != nil {
					return
				}
				time.Sleep(10 * time.Second)
			}
		}()
		log.Println("[*] Waiting for messages")
	receiving:
		for {
			select {
			case err = <-notify:
				log.Printf("An error occured while interacting with the broker, retrying in 30 seconds: %s", err)
				time.Sleep(30 * time.Second)
				break receiving
			case delivery := <-deliveryChan:
				var cl Calculation
				err := json.Unmarshal(delivery.Body, &cl)
				if err != nil {
					log.Println("Received invalid message")
					continue
				}
				if cl.Status != "in progress" {
					log.Println("Invalid data")
					continue
				}
				operations, err := getSettings()
				if err != nil {
					log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
					time.Sleep(30 * time.Second)
					continue
				}
				log.Printf("Evaluating %s", cl.Expression)
				res, err := evaluateComplexExpression(cl.Expression, numGoroutines, operations)
				if err != nil {
					log.Printf("Error occured while parsing %s", cl.Expression)
					query := `UPDATE Calculations SET status = $1 WHERE id = $2`
					_, err := db.Exec(query, "failed", cl.ID)
					if err != nil {
						log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
						time.Sleep(30 * time.Second)
						continue
					}
				} else {
					log.Printf("Done evaluating %s, result - %.2f", cl.Expression, res)
					query := `UPDATE Calculations SET status = $1, answer = $2 WHERE id = $3`
					_, err := db.Exec(query, "ok", res, cl.ID)
					if err != nil {
						log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
						time.Sleep(30 * time.Second)
						continue
					}
				}
				delivery.Ack(false)
			default:
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func main() {
	log.Println("Agent is starting...")
	numGoroutines := 5
	if len(os.Args) > 1 {
		val, err := strconv.Atoi(os.Args[1])
		if err != nil {
			log.Println("An error occured while parsing arguments, starting with default settings")
		} else {
			numGoroutines = val
		}
	}
	log.Printf("Agent started with %d working goroutines", numGoroutines)
	initDB()
	defer db.Close()
	agent := &Agent{Last_Seen: time.Now().Local().Format("01/02/2006 15:04:05"), Status: "active", Goroutines: numGoroutines}
	for {
		err := insertAgentIntoDB(agent)
		if err != nil {
			log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		agents, err := getAgentsFromDB()
		if err != nil {
			log.Printf("An error occured while interacting with the db, retrying in 30 seconds: %s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		agent.ID = agents[0].ID
		break
	}
	var forever chan interface{}
	go connectToRMQ(agent.ID, numGoroutines)
	<-forever
}
