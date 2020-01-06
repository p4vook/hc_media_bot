package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	"time"
	"unicode"
)

const d = 5 * 1000 * 1000 // sleep duration in nanoseconds

type link struct {
	Address string
	Enabled bool
}

type db struct {
	Hashes []uint64
	Urls   []link
	Ids    []int64
}

var bot *tgbotapi.BotAPI
var parser *gofeed.Parser
var feeds []*gofeed.Feed
var w *os.File
var hashes treeset.Set
var database db

func lookup(envName string) string {
	res, ok := os.LookupEnv(envName)
	if !ok {
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
	} else if r == '&' || r == '+' {
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
	for _, r := range category {
		x := replacement(r)
		if (x == "_" && res[len(res)-1] != '_') || x != "_" {
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

func formatItem(feed *gofeed.Feed, itemNumber int) *string {
	item := feed.Items[itemNumber]
	res := "[" + html.EscapeString(feed.Title) + "]\n<b>" +
		html.EscapeString(item.Title) + "</b>\n" +
		html.EscapeString(formatCategories(item)) + "\n\n" +
		"<a href=\"" + html.EscapeString(item.Link) + "\">Читать</a>"
	return &res
}

func sendItem(chatID int64, feed *gofeed.Feed, itemNumber int) bool {
	if itemNumber < len(feed.Items) {
		msg := tgbotapi.NewMessage(chatID, *formatItem(feed, itemNumber))
		msg.ParseMode = "HTML"
		_, _ = bot.Send(msg)
		return true
	} else {
		return false
	}
}

func removeGetArgs(u string) string {
	parsed, _ := url.Parse(u)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func filter(item *gofeed.Item) bool {
	hasher := fnv.New64a()
	_, _ = io.WriteString(hasher, removeGetArgs(item.Link))
	hash := hasher.Sum64()
	res := hashes.Contains(hash)
	if !res {
		hashes.Add(hash)
		_, _ = w.WriteString("+ h " + strconv.FormatUint(hash, 10) + "\n")
		_ = w.Sync()
		return true
	}
	return false
}

func update() {
	for i := 0; i < len(database.Urls); i += 1 {
		if database.Urls[i].Enabled {
			var err error
			response, err := http.Get(database.Urls[i].Address)
			if err != nil || response.StatusCode != http.StatusOK {
				continue
			}
			feeds[i], err = parser.Parse(response.Body)
			if err == nil {
				for itemNumber := len(feeds[i].Items) - 1; itemNumber >= 0; itemNumber -= 1 {
					if filter(feeds[i].Items[itemNumber]) {
						for idNumber := 0; idNumber < len(database.Ids); idNumber += 1 {
							sendItem(database.Ids[idNumber], feeds[i], itemNumber)
						}
					}
				}
			} else {
				log.Println(database.Urls[i].Address + " пидарасы!")
			}
		}
	}
}

func evolve() {
	data, err := ioutil.ReadFile("db.json")
	if err == nil {
		err = json.Unmarshal(data, &database)
	}
	n := len(database.Urls)
	hashes = *treeset.NewWith(uint64Comp)
	for _, hash := range database.Hashes {
		hashes.Add(hash)
	}
	file, err := os.Open("evolution.txt")
	if err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			splitted := strings.Split(scanner.Text(), " ")
			if splitted[0] == "+" {
				if splitted[1] == "h" {
					res, _ := strconv.ParseUint(splitted[2], 10, 64)
					hashes.Add(res)
					database.Hashes = append(database.Hashes, res)
				} else if splitted[1] == "u" {
					database.Urls = append(database.Urls, link{splitted[2], true})
					n += 1
				} else if splitted[1] == "i" {
					res, _ := strconv.ParseInt(splitted[2], 10, 64)
					database.Ids = append(database.Ids, res)
				}
			}
		}
	}
	feeds = make([]*gofeed.Feed, n)
	data, err = json.Marshal(database)
	file, err = os.Create("db.json")
	if err != nil {
		log.Panic(err)
	}
	_, _ = file.Write(data)
	_ = file.Sync()
}

func startPolling() {
	for {
		time.Sleep(d)
		go update()
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
        if update.Message != nil {
            chatId := update.Message.Chat.ID
            if update.Message.IsCommand() {
                args := update.Message.CommandArguments()
                switch update.Message.Command() {
                case "add_chat_id":
                    res, err := strconv.ParseInt(args, 10, 64)
                    if err == nil {
                        msg, err := bot.Send(tgbotapi.NewMessage(res, "Test"))
                        if err == nil {
                            fmt.Println(msg.MessageID)
                            tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)
                            database.Ids = append(database.Ids, res)
                            _, _ = w.WriteString("+ i " + strconv.FormatInt(res, 10) + "\n")
                            _ = w.Sync()
                            _, _ = bot.Send(tgbotapi.NewMessage(chatId, "Done!"))
                        } else {
                            _, _ = bot.Send(tgbotapi.NewMessage(chatId, "Check that bot has access to this chat"))
                        }
                    }
                case "start":
                    msg := tgbotapi.NewMessage(chatId, "Hi!")
                    _, _ = bot.Send(msg)
                case "get_ith":
                    splitted := strings.Split(args, " ")
                    if len(splitted) > 1 {
                        res1, err1 := strconv.Atoi(splitted[0])
                        res2, err2 := strconv.Atoi(splitted[1])
                        if err1 == nil && err2 == nil && res1 < len(feeds) && sendItem(chatId, feeds[res1], res2) {
                        } else {
                            msg := tgbotapi.NewMessage(chatId, "Check arguments")
                            _, _ = bot.Send(msg)
                        }
                    } else {
                        _, _ = bot.Send(tgbotapi.NewMessage(chatId, "Not a number"))
                    }
                case "add_feed":
                    if _, err := url.Parse(args); err == nil {
                        feed, err := parser.ParseURL(args)
                        if err == nil {
                            feeds = append(feeds, feed)
                            database.Urls = append(database.Urls, link{args, true})
                            _, _ = w.WriteString("+ u " + args + "\n")
                            _ = w.Sync()
                            _, _ = bot.Send(tgbotapi.NewMessage(chatId, "Done! New feed index: "+strconv.Itoa(len(feeds)-1)))
                        } else {
                            _, _ = bot.Send(tgbotapi.NewMessage(chatId, "Check that URL provides valid RSS/Atom feed"))
                        }
                    } else {
                        msg := tgbotapi.NewMessage(chatId, "Please send me an URL")
                        _, _ = bot.Send(msg)
                    }
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
	bot, err = tgbotapi.NewBotAPIWithClient(lookup("TOKEN"), client)
	if err != nil {
		log.Panic(err)
	}
	feeds = []*gofeed.Feed{}
	parser = gofeed.NewParser()
	evolve()
	w, err = os.Create("evolution.txt")
	go updateHandler()
	startPolling()
}
