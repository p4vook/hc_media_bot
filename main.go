package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/emirpasic/gods/sets/treeset"
	"github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/mmcdole/gofeed"
	"golang.org/x/net/proxy"
	"hash/fnv"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const d = 60 // sleep duration in seconds

type link struct {
	Address string
	Enabled bool
}

type db struct {
	Hashes []uint64
	Urls   []link
	Ids    []int64
}

type safeDB struct {
	Database db
	Mux Sync.RWMutex
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

func updateFeeds() {
	feeds.Mux.Lock()
	safeDatabase.Mux.RLock()
	for i := 0; i < len(safeDatabase.database.Urls); i += 1 {
		if safeDatabase.database.Urls[i].Enabled {
			response, err := http.Get(safeDatabase.database.Urls[i].Address)
			if err != nil || response.StatusCode != http.StatusOK {
				log.Println("Error when fetching URL "+safeDatabase.database.Urls[i].Address+": ", err)
				continue
			}
			feeds.FeedArray[i], err = parser.Parse(response.Body)
			if err == nil {
				for itemNumber := len(feeds.FeedArray[i].Items) - 1; itemNumber >= 0; itemNumber -= 1 {
					if filter(feeds.FeedArray[i].Items[itemNumber]) {
						for idNumber := 0; idNumber < len(safeDatabase.database.Ids); idNumber += 1 {
							sendItem(safeDatabase.database.Ids[idNumber], feeds.FeedArray[i], itemNumber)
						}
					}
				}
			} else {
				log.Println("Invalid RSS on address " + safeDatabase.database.Urls[i].Address)
			}
		}
	}
	defer safeDatabase.Mux.Unlock()
	defer feeds.Mux.RUnlock()
}

func evolve() {
	safeDatabase.Mux.Lock()
	data, err := ioutil.ReadFile("db.json")
	if err == nil {
		err = json.Unmarshal(data, &safeDatabase.database)
	}
	n := len(safeDatabase.database.Urls)
	hashes = *treeset.NewWith(uint64Comp)
	for _, hash := range safeDatabase.database.Hashes {
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
							safeDatabase.database.Hashes = append(safeDatabase.database.Hashes, res)
						} else if split[1] == "u" {
							safeDatabase.database.Urls = append(safeDatabase.database.Urls, link{split[2], true})
							n += 1
						} else if split[1] == "i" {
							res, _ := strconv.ParseInt(split[2], 10, 64)
							safeDatabase.database.Ids = append(safeDatabase.database.Ids, res)
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
	data, err = json.Marshal(safeDatabase.database)
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

func startPolling() {
	for {
		go updateFeeds()
		time.Sleep(time.Second * d)
	}
}

func updateHandler() {
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
				args := update.Message.CommandArguments()
				switch update.Message.Command() {
				case "add_chat_id":
					res, err := strconv.ParseInt(args, 10, 64)
					if err == nil {
						msg, err := bot.Send(tgbotapi.NewMessage(res, "Test"))
						if err == nil {
							_, _ = bot.DeleteMessage(tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID))
							safeDatabase.Mux.Lock()
							safeDatabase.database.Ids = append(safeDatabase.database.Ids, res)
							safeDatabase.Mux.Unlock()
							_, _ = w.WriteString("+ i " + strconv.FormatInt(res, 10) + "\n")
							_ = w.Sync()
							_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Done!"))
						} else {
							_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Check that bot has access to this chat"))
						}
					}
				case "start":
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Hi!"))
				case "ping":
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Pong."))
				case "add_feed":
					if _, err := url.Parse(args); err == nil {
						feed, err := parser.ParseURL(args)
						if err == nil {
							feeds.Mux.Lock()
							feeds.FeedArray = append(feeds.FeedArray, feed)
							feeds.Mux.Unlock()
							safeDatabase.Mux.Lock()
							safeDatabase.database.Urls = append(safeDatabase.database.Urls, link{args, true})
							safeDatabase.Mux.Unlock()
							_, _ = w.WriteString("+ u " + args + "\n")
							err = w.Sync()
							if err != nil {
								log.Panic(err)
							}
							go updateFeeds()
							_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Done!"))
						} else {
							_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Check that URL provides valid RSS/Atom feed."))
						}
					} else {
						msg := tgbotapi.NewMessage(chatId, "Please send me an URL")
						_, _ = bot.Send(msg)
					}
				case "update_feeds":
					go updateFeeds()
					_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Updating feeds..."))
				}
			}
		}
	}
}

func main() {
	auth := proxy.Auth{User: lookup("PROXY_USERNAME"), Password: lookup("PROXY_PASSWORD")}
	dialer, err := proxy.SOCKS5("tcp", lookup("SOCKS_PROXY_URL"), &auth, proxy.Direct)
	if err != nil {
		log.Fatal("Invalid SOCKS5 proxy URL")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) { return dialer.Dial(network, addr) }}
	client := &http.Client{Transport: transport}
	log.Println("Creating bot API...")
	bot, err = tgbotapi.NewBotAPIWithClient(lookup("TOKEN"), client)
	if err != nil {
		log.Panic(err)
	}
	parser = gofeed.NewParser()
	log.Println("Evolving db...")
	evolve()
	w, err = os.Create("evolution.txt")
	go updateHandler()
	log.Println("Starting polling...")
	startPolling()
}
