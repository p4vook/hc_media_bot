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
	"sort"
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

type ItemFormatOptions struct {
	CategoriesMap	map[string]string	`yaml:"categoriesMap"`
	LinkOptions struct {
		IncludeQueryString	bool	`yaml:"includeQueryString"`
		LinkText		string	`yaml:"linkText"`
	}					`yaml:"linkOptions"`
}

type HashingOptions struct {
	IncludeContent		bool	`yaml:"includeContent"`
	IncludeQueryString	bool	`yaml:"includeQueryString"`
}

type Feed struct {
	URL			string
	ItemFormatOptions	ItemFormatOptions	`yaml:"itemFormatOptions"`
	HashingOptions		HashingOptions		`yaml:"HashingOptions"`
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
	if unicode.IsLetter(r) || unicode.IsNumber(r) {
		return string(r)
	}
	// punctuation, dash
	if unicode.Is(unicode.Pd, r) {
		return string("_")
	}
	if unicode.IsSpace(r) || r == '&' || r == '+' || r == ';' {
		return string("_")
	}
	return string("")
}

func toHashTag(category string) string {
	if len(category) == 0 {
		return ""
	}
	res := "#"
	if unicode.IsNumber(rune(category[0])) {
		res += "_"
	}
	for i, r := range category {
		x := replacement(r)
		if x != "_" {
			res += x
		} else {
			// no consequent underscores, no underscores in the beginning
			// and in the end
			if (i != 0 && i != len(category) - 1 && res[len(res) - 1] != '_') {
				res += x
			}
		}
	}
	if res != "#" {
		return res
	} else {
		return ""
	}
}

func stringSliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if s == v {
			return true
		}
	}
	return false
}

func (formatOpts *ItemFormatOptions) formatCategories(item *gofeed.Item) string {
	hashtags := []string{}
	for _, category := range item.Categories {
		category = strings.ToLower(category)
		for k, v := range formatOpts.CategoriesMap {
			category = strings.Replace(category, k, v, -1)
		}
		hashtag := toHashTag(category)
		if hashtag != "" && !stringSliceContains(hashtags, hashtag) {
			hashtags = append(hashtags, hashtag)
		}
	}
	sort.Strings(hashtags)
	res := ""
	for i, hashtag := range hashtags {
		res += hashtag
		if i != len(hashtags) - 1 {
			res += " "
		}
	}
	return res
}

func removeQueryString(u string) (string, error) {
	parsed, err := url.Parse(u)
	if err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), nil
	} else {
		return "", err
	}
}

func (formatOpts *ItemFormatOptions) formatLink(link string) string {
	result := link
	if !formatOpts.LinkOptions.IncludeQueryString {
		var err error
		result, err = removeQueryString(result)
		if err != nil {
			log.Printf("Invalid item link: %s: %v\n", link, err)
			result = link
		}
	}
	if formatOpts.LinkOptions.LinkText != "" {
		return "<a href=\"" + html.EscapeString(result) +
			"\">" + html.EscapeString(formatOpts.LinkOptions.LinkText) + "</a>"
	}
	return html.EscapeString(result)
}

func (formatOpts *ItemFormatOptions) formatItem(feed *gofeed.Feed, itemNumber int) string {
	item := feed.Items[itemNumber]
	res := "[" + html.EscapeString(feed.Title) + "]\n<b>" +
		html.EscapeString(item.Title) + "</b>\n" +
		html.EscapeString(formatOpts.formatCategories(item)) + "\n\n" +
		formatOpts.formatLink(item.Link)
	return res
}

func sendItem(chatID int64, feed *gofeed.Feed, feedOpts *Feed, itemNumber int) bool {
	if feed != nil && feed.Items != nil && itemNumber < len(feed.Items) {
		msg := tgbotapi.NewMessage(chatID, feedOpts.ItemFormatOptions.formatItem(feed, itemNumber))
		msg.ParseMode = "HTML"
		_, _ = bot.Send(msg)
		return true
	} else {
		return false
	}
}

func (opts *HashingOptions) filter(item *gofeed.Item) bool {
	hash64 := fnv.New64a()
	toWrite := item.Link
	if !opts.IncludeQueryString {
		withoutQueryString, err := removeQueryString(item.Link)
		if err != nil {
			fmt.Printf("Error: invalid item link: %s: %v\n", item.Link, err)
			toWrite = ""
		}
		toWrite = withoutQueryString
	}
	if opts.IncludeContent {
		toWrite += "\n"
		toWrite += item.Content
	}
	_, err := io.WriteString(hash64, toWrite)
	if err != nil {
		fmt.Printf("Error: failed to hash item: %v\n", err)
		return true
	}
	hash := hash64.Sum64()
	res := hashes.Contains(hash)
	if !res {
		hashes.Add(hash)
		_, _ = w.WriteString("+ h " + strconv.FormatUint(hash, 10) + "\n")
		_ = w.Sync()
		return true
	}
	return false
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
				if feed.HashingOptions.filter(item) {
					for _, chatId := range config.ChatIds {
						sendItem(chatId, parsedFeed, &feed, itemNumber)
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
