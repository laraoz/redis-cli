package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"reflect"
	"io/ioutil"

	"github.com/go-redis/redis"
	"github.com/peterh/liner"
)

var (
	hostname    = flag.String("h", getEnv("REDIS_HOST", "127.0.0.1"), "Server hostname")
	port        = flag.String("p", getEnv("REDIS_PORT", "6379"), "Server server port")
	socket      = flag.String("s", "", "Server socket. (overwrites hostname and port)")
	dbn         = flag.Int("n", 0, "Database number(default 0)")
	auth        = flag.String("a", "", "Password to use when connecting to the server")
	outputRaw   = flag.Bool("raw", false, "Use raw formatting for replies")
	showWelcome = flag.Bool("welcome", false, "show welcome message, mainly for web usage via gotty")
)

var (
	mode int
	line        *liner.State
	client *redis.ClusterClient
	historyPath = path.Join(os.Getenv("HOME"), ".gorediscli_history") // $HOME/.gorediscli_history
)

//output
const (
	stdMode = iota
	rawMode
)

func main() {
	flag.Parse()

	if *outputRaw {
		mode = rawMode
	} else {
		mode = stdMode
	}

	// Start interactive mode when no command is provided
	if flag.NArg() == 0 {
		repl()
	}

	noninteractive(flag.Args())
}

func getEnv(key string, defaultValue string) string {
	value, found := os.LookupEnv(key)
	if !found {
		return defaultValue
	}
	return value
}

// Read-Eval-Print Loop
func repl() {
	line = liner.NewLiner()
	defer line.Close()
	line.SetCtrlCAborts(true)

	setCompletionHandler()
	loadHistory()
	defer saveHistory()

	reg, _ := regexp.Compile(`'.*?'|".*?"|\S+`)
	prompt := ""

	cliConnect()

	if *showWelcome {
		showWelcomeMsg()
	}

	for {
		addr := addr()
		if *dbn > 0 && *dbn < 16 {
			prompt = fmt.Sprintf("%s[%d]> ", addr, *dbn)
		} else {
			prompt = fmt.Sprintf("%s> ", addr)
		}

		cmd, err := line.Prompt(prompt)
		if err != nil {
			fmt.Printf("%s\n", err.Error())
			return
		}

		cmds := reg.FindAllString(cmd, -1)
		if len(cmds) == 0 {
			continue
		} else {
			
			appendHistory(cmds)

			cmd := strings.ToLower(cmds[0])
			if cmd == "help" || cmd == "?" {
				printHelp(cmds)
			} else if cmd == "quit" || cmd == "exit" {
				os.Exit(0)
			} else if cmd == "clear" {
				println("Please use Ctrl + L instead")
			} else if cmd == "connect" {
				reconnect(cmds[1:])
			} else if cmd == "mode" {
				switchMode(cmds[1:])
			} else {
				cliSendCommand(cmds...)
			}
		}
	}
}

func appendHistory(cmds []string) {
	// make a copy of cmds
	cloneCmds := make([]string, len(cmds))
	for i, cmd := range cmds {
		cloneCmds[i] = cmd
	}

	// for security reason, hide the password with ******
	if len(cloneCmds) == 2 && strings.ToLower(cloneCmds[0]) == "auth" {
		cloneCmds[1] = "******"
	}
	if len(cloneCmds) == 4 && strings.ToLower(cloneCmds[0]) == "connect" {
		cloneCmds[3] = "******"
	}
	line.AppendHistory(strings.Join(cloneCmds, " "))
}

func cliSendCommand(cmds ...string) {
	cliConnect()

	if len(cmds) == 0 {
		return
	}

	loadedScript := false
	if len(cmds) > 1 && cmds[1] == "--script" {
		content, err := ioutil.ReadFile(cmds[2])
		if err != nil {
			fmt.Printf("(error) %s\n", err.Error())
			return
		}
		cmds[2] = string(content)
	
		loadedScript = true
	}

	args := make([]interface{}, len(cmds))
	x := 0
	for i := range args {
		if loadedScript && i == 1 {
			continue
		}
		args[x] = strings.Trim(cmds[i], "\"'")
		x = x + 1
	}

	cmd := strings.ToLower(cmds[0])
	
	r, err := client.Do(args...).Result()
	if err == nil && strings.ToLower(cmd) == "select" {
		*dbn, _ = strconv.Atoi(cmds[1])
	}
	if err != nil {
		fmt.Printf("(error) %s", err.Error())
	} else {
		if cmd == "info" {
			printInfo(r)
		} else {
			printReply(0, r, mode)
		}

		if cmd == "eval" {
			fmt.Printf("\nSize of result: %v", SizeOf(r))
		} 
	}

	fmt.Printf("\n")
}

func cliConnect() {
	if client == nil {
		addr := addr()
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        []string{addr},
			Password:     *auth,
			TLSConfig:    &tls.Config{},
			PoolSize:     3,
			DialTimeout:  time.Second * 10,
			ReadTimeout:  time.Second * 10,
			WriteTimeout: time.Second * 10,
		})

		sendPing(client)
		sendSelect(client, *dbn)
	}
}

func reconnect(args []string) {
	if len(args) < 2 {
		fmt.Println("(error) invalid connect arguments. At least provides host and port.")
		return
	}

	h := args[0]
	p := args[1]

	var auth string
	if len(args) > 2 {
		auth = args[2]
	}

	if h != "" && p != "" {
		addr := fmt.Sprintf("%s:%s", h, p)
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        []string{addr},
			Password:     auth,
			TLSConfig:    &tls.Config{},
			PoolSize:     3,
			DialTimeout:  time.Second * 10,
			ReadTimeout:  time.Second * 10,
			WriteTimeout: time.Second * 10,
		})
	}

	if err := sendPing(client); err != nil {
		return
	}

	// change prompt
	hostname = &h
	port = &p

	if auth != "" {
		err := sendAuth(client, auth)
		if err != nil {
			return
		}
	}

	fmt.Printf("connected %s:%s successfully \n", h, p)
}

func switchMode(args []string) {
	if len(args) != 1 {
		fmt.Println("invalid args. Should be MODE [raw|std]")
		return
	}

	m := strings.ToLower(args[0])
	if m != "raw" && m != "std" {
		fmt.Println("invalid args. Should be MODE [raw|std]")
		return
	}

	switch m {
	case "std":
		mode = stdMode
	case "raw":
		mode = rawMode
	}

	return
}

func addr() string {
	var addr string
	if len(*socket) > 0 {
		addr = *socket
	} else {
		addr = fmt.Sprintf("%s:%s", *hostname, *port)
	}
	return addr
}

func noninteractive(args []string) {
	cliSendCommand(args...)
}

func printInfo(reply interface{}) {
	switch reply := reply.(type) {
	case []byte:
		fmt.Printf("%s", reply)
	//some redis proxies don't support this command.
	case error:
		fmt.Printf("(error) %s", reply.Error())
	}
}

func printReply(level int, reply interface{}, mode int) {
	switch mode {
	case stdMode:
		printStdReply(level, reply)
	case rawMode:
		printRawReply(level, reply)
	default:
		printStdReply(level, reply)
	}

}

func printStdReply(level int, reply interface{}) {
	switch reply := reply.(type) {
	case int64:
		fmt.Printf("(integer) %d", reply)
	case string:
		fmt.Printf("%s", reply)
	case []byte:
		fmt.Printf("%q", reply)
	case nil:
		fmt.Printf("(nil)")
	case error:
		fmt.Printf("%s\n", reply.Error())
	case []interface{}:
		for i, v := range reply {
			if i != 0 {
				fmt.Printf("%s", strings.Repeat(" ", level*4))
			}

			s := fmt.Sprintf("%d) ", i+1)
			fmt.Printf("%-4s", s)

			printStdReply(level+1, v)
			if i != len(reply)-1 {
				fmt.Printf("\n")
			}
		}
	default:
		fmt.Printf("Unknown reply type: %+v", reply)
	}
}

func printRawReply(level int, reply interface{}) {
	switch reply := reply.(type) {
	case int64:
		fmt.Printf("%d", reply)
	case string:
		fmt.Printf("%s", reply)
	case []byte:
		fmt.Printf("%s", reply)
	case nil:
		// do nothing
	case error:
		fmt.Printf("%s\n", reply.Error())
	case []interface{}:
		for i, v := range reply {
			if i != 0 {
				fmt.Printf("%s", strings.Repeat(" ", level*4))
			}

			printRawReply(level+1, v)
			if i != len(reply)-1 {
				fmt.Printf("\n")
			}
		}
	default:
		fmt.Printf("Unknown reply type: %+v", reply)
	}
}

func printGenericHelp() {
	msg :=
		`redis-cli
Type:	"help <command>" for help on <command>
	`
	fmt.Println(msg)
}

func printCommandHelp(arr []string) {
	fmt.Println()
	fmt.Printf("\t%s %s \n", arr[0], arr[1])
	fmt.Printf("\tGroup: %s \n", arr[2])
	fmt.Println()
}

func printHelp(cmds []string) {
	args := cmds[1:]
	if len(args) == 0 {
		printGenericHelp()
	} else if len(args) > 1 {
		fmt.Println()
	} else {
		cmd := strings.ToUpper(args[0])
		for i := 0; i < len(helpCommands); i++ {
			if helpCommands[i][0] == cmd {
				printCommandHelp(helpCommands[i])
			}
		}
	}
}

func sendSelect(client *redis.ClusterClient, index int) {
	if index == 0 {
		// do nothing
		return
	}
	if index > 16 || index < 0 {
		index = 0
		fmt.Println("index out of range, should less than 16")
	}
	_, err := client.Do("SELECT", index).Result()
	if err != nil {
		fmt.Printf("%s\n", err.Error())
	}
}

func sendAuth(client *redis.ClusterClient, passwd string) error {
	if passwd == "" {
		// do nothing
		return nil
	}

	resp, err := client.Do("AUTH", passwd).Result()
	if err != nil {
		fmt.Printf("(error) %s\n", err.Error())
		return err
	}

	switch resp := resp.(type) {
	case error:
		fmt.Printf("(error) %s\n", resp.Error())
		return resp
	}

	return nil
}

func sendPing(client *redis.ClusterClient) error {
	_, err := client.Do("PING").Result()
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		return err
	}
	return nil
}

func setCompletionHandler() {
	line.SetCompleter(func(line string) (c []string) {
		for _, i := range helpCommands {
			if strings.HasPrefix(i[0], strings.ToUpper(line)) {
				c = append(c, i[0])
			}
		}
		return
	})
}

func loadHistory() {
	if f, err := os.Open(historyPath); err == nil {
		line.ReadHistory(f)
		f.Close()
	}
}

func saveHistory() {
	if f, err := os.Create(historyPath); err != nil {
		fmt.Printf("Error writing history file: %s", err.Error())
	} else {
		line.WriteHistory(f)
		f.Close()
	}
}

func showWelcomeMsg() {
	welcome := `
	Welcome to redis-cli online.
	You can switch to different redis instance with the CONNECT command. 
	Usage: CONNECT host port [auth]

	Switch output mode with MODE command. 

	Usage: MODE [std | raw]
	`
	fmt.Println(welcome)
}




// SizeOf returns the size of 'v' in bytes.
// If there is an error during calculation, Of returns -1.
func SizeOf(v interface{}) int {
	cache := make(map[uintptr]bool) // cache with every visited Pointer for recursion detection
	return sizeOf(reflect.Indirect(reflect.ValueOf(v)), cache)
}

// sizeOf returns the number of bytes the actual data represented by v occupies in memory.
// If there is an error, sizeOf returns -1.
func sizeOf(v reflect.Value, cache map[uintptr]bool) int {

	switch v.Kind() {

	case reflect.Array:
		fallthrough
	case reflect.Slice:
		// return 0 if this node has been visited already (infinite recursion)
		if v.Kind() != reflect.Array && cache[v.Pointer()] {
			return 0
		}
		if v.Kind() != reflect.Array {
			cache[v.Pointer()] = true
		}
		sum := 0
		for i := 0; i < v.Len(); i++ {
			s := sizeOf(v.Index(i), cache)
			if s < 0 {
				return -1
			}
			sum += s
		}
		return sum + int(v.Type().Size())

	case reflect.Struct:
		sum := 0
		for i, n := 0, v.NumField(); i < n; i++ {
			s := sizeOf(v.Field(i), cache)
			if s < 0 {
				return -1
			}
			sum += s
		}
		return sum

	case reflect.String:
		return len(v.String()) + int(v.Type().Size())

	case reflect.Ptr:
		// return Ptr size if this node has been visited already (infinite recursion)
		if cache[v.Pointer()] {
			return int(v.Type().Size())
		}
		cache[v.Pointer()] = true
		if v.IsNil() {
			return int(reflect.New(v.Type()).Type().Size())
		}
		s := sizeOf(reflect.Indirect(v), cache)
		if s < 0 {
			return -1
		}
		return s + int(v.Type().Size())

	case reflect.Bool,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Int,
		reflect.Chan,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return int(v.Type().Size())

	case reflect.Map:
		// return 0 if this node has been visited already (infinite recursion)
		if cache[v.Pointer()] {
			return 0
		}
		cache[v.Pointer()] = true
		sum := 0
		keys := v.MapKeys()
		for i := range keys {
			val := v.MapIndex(keys[i])
			// calculate size of key and value separately
			sv := sizeOf(val, cache)
			if sv < 0 {
				return -1
			}
			sum += sv
			sk := sizeOf(keys[i], cache)
			if sk < 0 {
				return -1
			}
			sum += sk
		}
		return sum + int(v.Type().Size())

	case reflect.Interface:
		return sizeOf(v.Elem(), cache) + int(v.Type().Size())
	}

	return -1
}