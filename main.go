package main

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/go-co-op/gocron"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// Maintained by Yigit Tarik Hakan

type JobParameters struct {
	AppName     string
	MinReplicas int
	MaxReplicas int
	DateTime    time.Time
}

type ScheduledJob struct {
	ID        int
	JobParams JobParameters
}

type Profile struct {
	Name        string `json:"name"`
	MinReplicas int    `json:"min_replicas"`
	MaxReplicas int    `json:"max_replicas"`
}

type App struct {
	Name     string    `json:"name"`
	Alias    string    `json:"alias"`
	Profiles []Profile `json:"profiles"`
}

type Task struct {
	ID          string    `json:"id"`
	Command     string    `json:"command"`
	LastRun     time.Time `json:"last_run"`
	NextRun     time.Time `json:"next_run"`
	Schedule    string    `json:"schedule"`
	Status      string    `json:"status"`
	Username    string    `json:"username"`
	AppName     string    `json:"app_name"` // Ensure this matches your DynamoDB item structure
	MinReplicas int       `json:"min_replicas"`
	MaxReplicas int       `json:"max_replicas"`
}

var (
	location, _   = time.LoadLocation("Europe/Istanbul")
	scheduler     = gocron.NewScheduler(location)
	scheduledJobs []ScheduledJob
	jobMutex      sync.Mutex
	apps          []string
	log           = logrus.New()
	db            dynamodbiface.DynamoDBAPI
)

func initDynamoDB() {
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String("eu-west-1"),
	}))
	db = dynamodb.New(sess)
	checkDynamoDBConnectivity()
}

func checkDynamoDBConnectivity() {
	_, err := db.ListTables(&dynamodb.ListTablesInput{})
	if err != nil {
		log.Fatalf("Failed to connect to DynamoDB: %v", err)
	}
	log.Info("Connected to DynamoDB successfully")
}

var jwtSecret = []byte("your_secret_key")

type contextKey string

const usernameKey contextKey = "username"

func generateToken(username string) (string, error) {
	token := jwt.New(jwt.SigningMethodHS256)
	claims := token.Claims.(jwt.MapClaims)
	claims[string(usernameKey)] = username
	claims["exp"] = time.Now().Add(time.Hour).Unix() // Token expires in 1 hour

	return token.SignedString(jwtSecret)
}

func validateToken(tokenString string) (*jwt.Token, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})
	return token, err
}

func main() {
	initDynamoDB()

	if location == nil {
		log.Fatal("Failed to load time location.")
		return
	}

	if _, err := loadAppsFromDynamoDB(); err != nil {
		log.Fatalf("Failed to load Apps from DynamoDB: %v", err)
	}

	loadAndRegisterTasks()

	r := gin.Default()
	r.Use(corsMiddleware())

	r.POST("/login", LoginHandler)
	r.GET("/tasks", getTasksHandler)
	r.GET("/apps", jwtAuthMiddleware(), getApps)
	r.POST("/apps", jwtAuthMiddleware(), CreateAppHandler)
	r.DELETE("/tasks/:id", removeTaskHandler)
	r.POST("/schedule", jwtAuthMiddleware(), ScheduleHandler)

	currentTime := time.Now().In(location)
	log.Infof("Current time: %s", currentTime.Format(time.RFC3339))

	r.Run(":8080")
}

func ScheduleHandler(c *gin.Context) {
	log.Println("ScheduleHandler called")

	// Parse form values
	appName := c.PostForm("appName")
	minReplicas := c.PostForm("minReplicas")
	maxReplicas := c.PostForm("maxReplicas")
	dateTime := c.PostForm("datetime")

	// Validate inputs
	if appName == "" || minReplicas == "" || maxReplicas == "" || dateTime == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "All fields are required"})
		return
	}

	// Parse date and time
	scheduledTime, err := time.ParseInLocation("2006-01-02T15:04", dateTime, location)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid date and time format"})
		return
	}

	// Convert replica values to integers
	minReplicasInt, err := strconv.Atoi(minReplicas)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid number for minReplicas"})
		return
	}
	maxReplicasInt, err := strconv.Atoi(maxReplicas)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid number for maxReplicas"})
		return
	}

	// Prepare job parameters
	params := JobParameters{
		AppName:     appName,
		MinReplicas: minReplicasInt,
		MaxReplicas: maxReplicasInt,
		DateTime:    scheduledTime,
	}

	// Generate unique ID and add job to the schedule
	jobMutex.Lock()
	jobID := generateUniqueID()
	id, err := strconv.Atoi(jobID)
	if err != nil {
		jobMutex.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate job ID"})
		return
	}
	scheduledJobs = append(scheduledJobs, ScheduledJob{ID: id, JobParams: params})
	jobMutex.Unlock()

	// Log scheduled job
	log.Printf("Scheduled job: ID=%d, AppName=%s, MinReplicas=%d, MaxReplicas=%d, DateTime=%s\n",
		id, appName, minReplicasInt, maxReplicasInt, scheduledTime.Format(time.RFC3339))

	// Schedule the job using cron expression
	cronExpr := fmt.Sprintf("%d %d %d %d *", scheduledTime.Minute(), scheduledTime.Hour(), scheduledTime.Day(), scheduledTime.Month())
	_, err = scheduler.Cron(cronExpr).Do(runJob, params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error scheduling job"})
		return
	}

	// Start the scheduler
	log.Println("Starting scheduler")
	scheduler.StartAsync()

	// Redirect to success page
	c.Redirect(http.StatusSeeOther, "/?success=true")
}

func runJob(params JobParameters) {
	argocdServer := "test"
	argocdUser := "test"
	argocdAuthToken := "test"

	fmt.Printf("Running job with parameters: %+v\n", params)

	cmd1 := exec.Command("argocd", fmt.Sprintf("login %s --insecure --username %s --password %s", argocdServer, argocdUser, argocdAuthToken))
	time.Sleep(1000)

	cmd2 := exec.Command("argocd", fmt.Sprintf("app set %s -p autoscaling.minReplicas=%d -p autoscaling.maxReplicas=%d", params.AppName, params.MinReplicas, params.MaxReplicas))
	time.Sleep(1000)

	cmd3 := exec.Command("argocd", fmt.Sprintf("app sync %s", params.AppName))

	if err := cmd1.Run(); err != nil {
		log.Info("Error logging in to argoCD:", err)
	}
	if err := cmd2.Run(); err != nil {
		log.Info("Error while auto scaling:", err)
	}
	if err := cmd3.Run(); err != nil {
		log.Info("Error while sync:", err)
	}

	log.Infoln("Commands executed successfully!")
}

func generateUniqueID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "http://localhost:3000")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Next()
	}
}

func LoginHandler(c *gin.Context) {
	var loginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := c.BindJSON(&loginRequest); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid input"})
		return
	}

	username := loginRequest.Username
	password := loginRequest.Password

	if username == "" || password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Username and password are required"})
		return
	}

	token, valid := authenticateUser(username, password)
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"token": token})
}

func getTasksHandler(c *gin.Context) {
	log.Println("getTasksHandler called")

	result, err := db.Scan(&dynamodb.ScanInput{
		TableName: aws.String("devops-venus-tasks"),
	})
	if err != nil {
		log.Errorf("Error scanning DynamoDB table: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve tasks"})
		return
	}

	var tasks []Task
	for _, item := range result.Items {
		task := Task{}

		if appNameAttr, ok := item["AppName"]; ok && appNameAttr.S != nil {
			task.AppName = aws.StringValue(appNameAttr.S)
		}

		if commandAttr, ok := item["Command"]; ok && commandAttr.S != nil {
			task.Command = aws.StringValue(commandAttr.S)
		}

		if idAttr, ok := item["ID"]; ok && idAttr.S != nil {
			task.ID = aws.StringValue(idAttr.S)
		}

		if nextRunAttr, ok := item["NextRun"]; ok && nextRunAttr.S != nil {
			if nextRun, err := time.Parse(time.RFC3339, aws.StringValue(nextRunAttr.S)); err == nil {
				task.NextRun = nextRun
			} else {
				log.Errorf("Error parsing NextRun: %v", err)
			}
		}

		if lastRunAttr, ok := item["LastRun"]; ok && lastRunAttr.S != nil {
			if lastRun, err := time.Parse(time.RFC3339, aws.StringValue(lastRunAttr.S)); err == nil {
				task.LastRun = lastRun
			} else {
				log.Errorf("Error parsing LastRun: %v", err)
			}
		}

		if scheduleAttr, ok := item["Schedule"]; ok && scheduleAttr.S != nil {
			task.Schedule = aws.StringValue(scheduleAttr.S)
		}

		if statusAttr, ok := item["Status"]; ok && statusAttr.S != nil {
			task.Status = aws.StringValue(statusAttr.S)
		}

		if usernameAttr, ok := item["Username"]; ok && usernameAttr.S != nil {
			task.Username = aws.StringValue(usernameAttr.S)
		}

		tasks = append(tasks, task)
	}

	c.JSON(http.StatusOK, tasks)
}

func jwtAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := c.GetHeader("Authorization")
		if tokenString == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing token"})
			c.Abort()
			return
		}

		// Remove "Bearer " prefix if present
		if len(tokenString) > 7 && tokenString[:7] == "Bearer " {
			tokenString = tokenString[7:]
		}

		token, err := validateToken(tokenString)
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			c.Abort()
			return
		}

		claims := token.Claims.(jwt.MapClaims)
		username, ok := claims[string(usernameKey)].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token claims"})
			c.Abort()
			return
		}

		// Store username in context
		c.Set(string(usernameKey), username)

		c.Next()
	}
}

func validateUserInDynamoDB(username, password string) bool {
	input := &dynamodb.GetItemInput{
		TableName: aws.String("devops-venus-users"),
		Key: map[string]*dynamodb.AttributeValue{
			"Username": {S: aws.String(username)},
		},
	}

	result, err := db.GetItem(input)
	if err != nil {
		log.Errorf("Error fetching user from DynamoDB: %v", err)
		return false
	}

	if result.Item == nil {
		log.Warnf("No item found for username: %s", username)
		return false
	}

	storedPasswordHashAttr, ok := result.Item["Password"]
	if !ok || storedPasswordHashAttr.S == nil {
		log.Warnf("Password not found for username: %s", username)
		return false
	}

	storedPasswordHash := *storedPasswordHashAttr.S
	err = bcrypt.CompareHashAndPassword([]byte(storedPasswordHash), []byte(password))
	return err == nil
}

func authenticateUser(username, password string) (string, bool) {
	if validateUserInDynamoDB(username, password) {
		token, err := generateToken(username)
		if err != nil {
			log.Errorf("Error generating token: %v", err)
			return "", false
		}
		return token, true
	}
	return "", false
}

func loadAppsFromDynamoDB() ([]string, error) {
	result, err := db.Scan(&dynamodb.ScanInput{
		TableName: aws.String("devops-venus-apps"),
	})
	if err != nil {
		log.Errorf("Error scanning DynamoDB table: %v", err)
		return nil, err
	}

	if len(result.Items) == 0 {
		log.Warn("No items found in DynamoDB table.")
		return nil, nil // or return an empty slice if preferred
	}

	var loadedApps []string
	for _, item := range result.Items {
		if value, ok := item["Value"]; ok && value.S != nil {
			loadedApps = append(loadedApps, *value.S)
		} else {
			log.Warnf("Item missing 'Value' attribute or value is nil: %v", item)
		}
	}

	if len(loadedApps) == 0 {
		log.Warn("No valid apps found in DynamoDB table.")
	}

	return loadedApps, nil
}

func loadAndRegisterTasks() {
	jobMutex.Lock()
	defer jobMutex.Unlock()

	scheduler.Clear()

	for _, job := range scheduledJobs {
		durationUntilRun := time.Until(job.JobParams.DateTime)

		scheduler.Every(durationUntilRun.Seconds()).Seconds().Do(func(job ScheduledJob) {
			executeJob(job)
		}, job)
	}
}

func executeJob(job ScheduledJob) {
	fmt.Printf("Executing job with ID %d and app name %s\n", job.ID, job.JobParams.AppName)

	commandStr := fmt.Sprintf("some-command --app=%s --min-replicas=%d --max-replicas=%d",
		job.JobParams.AppName, job.JobParams.MinReplicas, job.JobParams.MaxReplicas)

	executeCommand(commandStr)

	taskID := strconv.Itoa(job.ID)

	updateTaskStatus(taskID, "completed")
}

func updateTaskStatus(taskID, status string) {
	_, err := db.UpdateItem(&dynamodb.UpdateItemInput{
		TableName: aws.String("devops-venus-tasks"),
		Key: map[string]*dynamodb.AttributeValue{
			"ID": {S: aws.String(taskID)},
		},
		UpdateExpression: aws.String("SET #status = :status"),
		ExpressionAttributeNames: map[string]*string{
			"#status": aws.String("Status"),
		},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":status": {S: aws.String(status)},
		},
	})
	if err != nil {
		log.Errorf("Failed to update task status: %v", err)
	}
}

func getApps(c *gin.Context) {
	log.Infof("Fetching apps, current options: %v", apps)

	var appList []App
	for _, appName := range apps {
		appList = append(appList, App{
			Name:     appName,
			Alias:    "alias_" + appName, // Placeholder for alias, adjust as necessary
			Profiles: []Profile{},        // Placeholder for profiles, adjust as necessary
		})
	}

	if len(appList) == 0 {
		log.Info("No apps found")
		c.JSON(http.StatusOK, gin.H{"message": "No apps found"})
		return
	}

	c.JSON(http.StatusOK, appList)
}

func removeTaskHandler(c *gin.Context) {
	id := c.Param("id")
	if err := removeTaskFromDB(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove task"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "Task removed"})
}

func removeTaskFromDB(id string) error {
	_, err := db.DeleteItem(&dynamodb.DeleteItemInput{
		TableName: aws.String("devops-venus-tasks"),
		Key: map[string]*dynamodb.AttributeValue{
			"ID": {S: aws.String(id)},
		},
	})
	return err
}

func executeCommand(commandStr string) {
	cmd := exec.Command("bash", "-c", commandStr)
	err := cmd.Run()
	if err != nil {
		log.Errorf("Error executing command %s: %v", commandStr, err)
	}
}

func CreateAppHandler(c *gin.Context) {
	var app App

	// Bind JSON to App struct
	if err := c.ShouldBindJSON(&app); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid input"})
		return
	}

	// Validate input
	if app.Name == "" || app.Alias == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Name and Alias are required"})
		return
	}

	// Prepare DynamoDB input
	item := map[string]*dynamodb.AttributeValue{
		"Name":  {S: aws.String(app.Name)},
		"Alias": {S: aws.String(app.Alias)},
		"Profiles": {
			L: convertProfilesToAttributeValues(app.Profiles),
		},
	}

	// Store the app in DynamoDB
	_, err := db.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String("devops-venus-apps"),
		Item:      item,
	})
	if err != nil {
		log.Errorf("Failed to create app in DynamoDB: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "App created successfully"})
}

// Helper function to convert profiles to DynamoDB AttributeValues
func convertProfilesToAttributeValues(profiles []Profile) []*dynamodb.AttributeValue {
	var items []*dynamodb.AttributeValue
	for _, profile := range profiles {
		items = append(items, &dynamodb.AttributeValue{
			M: map[string]*dynamodb.AttributeValue{
				"Name":        {S: aws.String(profile.Name)},
				"MinReplicas": {N: aws.String(strconv.Itoa(profile.MinReplicas))},
				"MaxReplicas": {N: aws.String(strconv.Itoa(profile.MaxReplicas))},
			},
		})
	}
	return items
}
