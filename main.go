package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/emirpasic/gods/sets/treeset"
	"gopkg.in/telegram-bot-api.v4"
	"github.com/mmcdole/gofeed"
	"hash/fnv"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"gopkg.in/yaml.v2"
)

type link struct {
	Address	string
	Enabled	bool
}

type Feed struct {
	URL		string
	CategoriesMap	map[string]string	`yaml:"categoriesMap"`
	ItemLinkOptions struct {
		HashWithTitle		bool	`yaml:"hashWithTitle"`
		RemoveQueryString	bool	`yaml:"removeQueryString"`
	}					`yaml:"itemLinkOptions"`
}

type Config struct {
	BotToken	string		`yaml:"botToken"`
	PollIntervalStr	string		`yaml:"pollInterval"`
	PollInterval	time.Duration
	Feeds		[]Feed
	ChatIds		[]int64		`yaml:"chatIds"`
}

type db struct {
	Hashes []uint64
	Urls   []link
	Ids    []int64
}

type safeDB struct {
	Database db
	Mux sync.RWMutex
}

type safeFeeds struct {
	FeedArray []*gofeed.Feed
	Mux       sync.Mutex
}

var bot *tgbotapi.BotAPI
var parser *gofeed.Parser
var w *os.File
var feeds safeFeeds
var hashes treeset.Set
var safeDatabase safeDB

func lookup(envName string) string {
	res, found := os.LookupEnv(envName)
	if !found {
		log.Fatal("Please set the " + envName + " environmental variable.")
	}
	return res
}

func uint64Comp(a, b interface{}) int {
	aUint := a.(uint64)
	bUint := b.(uint64)
	if aUint < bUint {
		return -1
	} else if aUint > bUint {
		return 1
	} else {
		return 0
	}
}

func replacement(r rune) string {
	var res string
	if (8210 <= int(r) && int(r) < 8214) || int(r) == 11834 || int(r) == 11835 || int(r) == 45 || int(r) == 32 {
		res = "_"
	} else if r == '!' || r == '?' || r == '(' || r == ')' || r == '\'' || r == '"' || r == '«' || r == '»' {
		res = ""
	} else if r == '&' || r == '+' || r == ';' {
		res = "_"
	} else if r == '#' {
		res = "sharp"
	} else if r == '.' {
		res = "dot"
	} else {
		res = string(unicode.ToLower(r))
	}
	return res
}

func toHashTag(category string) string {
	res := "#"
	category = strings.ReplaceAll(category, "*nix", "unix")
	category = strings.ReplaceAll(category, "c++", "cpp")
	for i, r := range category {
		x := replacement(r)
		if (x == "_" && res[len(res)-1] != '_' && i != 0 && i != len(category)-1) || x != "_" {
			res += x
		}
	}
	return res
}

func formatCategories(item *gofeed.Item) string {
	n := len(item.Categories)
	categories := make([]string, n)
	s := treeset.NewWithStringComparator()
	for i := 0; i < n; i += 1 {
		categories[i] = toHashTag(item.Categories[i])
	}
	for i := 0; i < n; i += 1 {
		if !s.Contains(categories[i]) {
			s.Add(categories[i])
		}
	}
	values := s.Values()
	res := ""
	for index := 0; index < len(values); index += 1 {
		res += values[index].(string) + " "
	}
	return res
}

func formatItem(feed *gofeed.Feed, itemNumber int) string {
	item := feed.Items[itemNumber]
	res := "[" + html.EscapeString(feed.Title) + "]\n<b>" +
		html.EscapeString(item.Title) + "</b>\n" +
		html.EscapeString(formatCategories(item)) + "\n\n" +
		"<a href=\"" + html.EscapeString(item.Link) + "\">Читать</a>"
	return res
}

func sendItem(chatID int64, feed *gofeed.Feed, itemNumber int) bool {
	if feed != nil && feed.Items != nil && itemNumber < len(feed.Items) {
		msg := tgbotapi.NewMessage(chatID, formatItem(feed, itemNumber))
		msg.ParseMode = "HTML"
		_, _ = bot.Send(msg)
		return true
	} else {
		return false
	}
}

func removeGetArgs(u string) (string, error) {
	parsed, err := url.Parse(u)
	if err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), nil
	} else {
		return "", err
	}
}

func filter(item *gofeed.Item) bool {
	hash64 := fnv.New64a()
	withoutGetArgs, err := removeGetArgs(item.Link)
	if err == nil {
		_, err = io.WriteString(hash64, withoutGetArgs)
		if err == nil {
			hash := hash64.Sum64()
			res := hashes.Contains(hash)
			if !res {
				hashes.Add(hash)
				_, _ = w.WriteString("+ h " + strconv.FormatUint(hash, 10) + "\n")
				_ = w.Sync()
				return true
			}
		}
		return false
	} else {
		return true
	}
}

func (config *Config) updateFeeds() {
	feeds.Mux.Lock()
	safeDatabase.Mux.RLock()
	log.Printf("Updating feeds\n")
	for _, feed := range config.Feeds {
		response, err := http.Get(feed.URL)
		if err != nil || response.StatusCode != http.StatusOK {
			if err == nil {
				err = fmt.Errorf("Bad status code: %d", response.StatusCode)
			}
			log.Printf("Error when fetching URL %s: %v\n", feed.URL, err)
			continue
		}
		parsedFeed, err := parser.Parse(response.Body)
		if err == nil {
			for itemNumber := len(parsedFeed.Items) - 1; itemNumber >= 0; itemNumber -= 1 {
				item := parsedFeed.Items[itemNumber]
				if filter(item) {
					for _, chatId := range config.ChatIds {
						sendItem(chatId, parsedFeed, itemNumber)
					}
				}
			}
		} else {
			log.Printf("Invalid RSS on address %s: %s\n", feed.URL, err)
		}
	}
	defer safeDatabase.Mux.RUnlock()
	defer feeds.Mux.Unlock()
}

func evolve() {
	safeDatabase.Mux.Lock()
	data, err := ioutil.ReadFile("db.json")
	if err == nil {
		err = json.Unmarshal(data, &safeDatabase.Database)
	}
	n := len(safeDatabase.Database.Urls)
	hashes = *treeset.NewWith(uint64Comp)
	for _, hash := range safeDatabase.Database.Hashes {
		hashes.Add(hash)
	}
	file, err := os.Open("evolution.txt")
	if err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			split := strings.Split(scanner.Text(), " ")
			if len(split) > 0 {
				if split[0] == "+" {
					if len(split) == 3 {
						if split[1] == "h" {
							res, _ := strconv.ParseUint(split[2], 10, 64)
							hashes.Add(res)
							safeDatabase.Database.Hashes = append(safeDatabase.Database.Hashes, res)
						} else if split[1] == "u" {
							safeDatabase.Database.Urls = append(safeDatabase.Database.Urls, link{split[2], true})
							n += 1
						} else if split[1] == "i" {
							res, _ := strconv.ParseInt(split[2], 10, 64)
							safeDatabase.Database.Ids = append(safeDatabase.Database.Ids, res)
						}
					} else {
						log.Println("Wrong number of arguments on this line: " + scanner.Text() + " in evolution.txt")
					}
				}
			} else {
				log.Println("Empty line when parsing evolution.txt")
			}
		}
	}
	feeds.Mux.Lock()
	feeds.FeedArray = make([]*gofeed.Feed, n)
	feeds.Mux.Unlock()
	data, err = json.Marshal(safeDatabase.Database)
	if err != nil {
		log.Panic(err)
	}
	file, err = os.Create("db.json")
	if err != nil {
		log.Panic(err)
	}
	_, err = file.Write(data)
	if err != nil {
		log.Panic(err)
	}
	err = file.Sync()
	if err != nil {
		log.Panic(err)
	}
	safeDatabase.Mux.Unlock()
}

func (conf *Config) startPolling() {
	for {
		go conf.updateFeeds()
		time.Sleep(conf.PollInterval)
	}
}

func (conf *Config) updateHandler() {
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 120
	updates, err := bot.GetUpdatesChan(updateConfig)
	if err != nil {
		log.Panic(err)
	}
	for update := range updates {
		if update.Message != nil && update.Message.Chat != nil {
			chatId := update.Message.Chat.ID
			if update.Message.IsCommand() {
				switch update.Message.Command() {
				case "start":
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Hi!"))
				case "ping":
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Pong."))
				case "update_feeds":
					go conf.updateFeeds()
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Updating feeds..."))
				}
			}
		}
	}
}

func parseConfig(in []byte) (*Config, error) {
	var result Config
	err := yaml.Unmarshal(in, &result)
	if err != nil {
		return nil, err
	}
	if result.PollIntervalStr != "" {
		interval, err := time.ParseDuration(result.PollIntervalStr)
		if err != nil {
			return nil, err
		}
		result.PollInterval = interval
	} else {
		result.PollInterval = time.Second * 60
	}
	return &result, err
}

func readConfig(filename string) (*Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return parseConfig(data)
}

func main() {
	config, err := readConfig("config.yaml") // TODO: command-line config argument
	if err != nil {
		log.Panic(err)
	}
	// add back proxy support ?
//	auth := proxy.Auth{User: lookup("PROXY_USERNAME"), Password: lookup("PROXY_PASSWORD")}
//	dialer, err := proxy.SOCKS5("tcp", lookup("SOCKS_PROXY_URL"), &auth, proxy.Direct)
//	if err != nil {
//		log.Fatal("Invalid SOCKS5 proxy URL")
//	}
//	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) { return dialer.Dial(network, addr) }}
//	client := &http.Client{Transport: transport}
	log.Println("Creating bot API...")
	bot, err = tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}
	parser = gofeed.NewParser()
	log.Println("Evolving db...")
	evolve()
	w, err = os.Create("evolution.txt")
	go config.updateHandler()
	log.Println("Starting polling...")
	config.startPolling()
}
