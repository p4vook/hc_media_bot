package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/emirpasic/gods/sets/treeset"
	"github.com/go-telegram-bot-api/telegram-bot-api"
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
	"time"
	"unicode"
)

const p = 75
const d = 10000000000

var bot *tgbotapi.BotAPI
var parser *gofeed.Parser
var feeds []*gofeed.Feed
var w *os.File

type db struct {
	Hashes []uint64
	Urls   []string
	Ids    []int64
}

var database db


func lookup(envName string) string {
	res, ok := os.LookupEnv(envName)
	if !ok {
		log.Fatal("Please set the " + envName + " environmental variable.")
	}
	return res
}

func replacement(r rune) *string {
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
	return &res
}

func toHashTag(category *string) *string{
	res := "#"
	*category = strings.ReplaceAll(*category, "*nix", "unix")
	*category = strings.ReplaceAll(*category, "c++", "cpp")
	for _, r := range *category {
		res += *replacement(r)
	}
	return &res
}


func getHash(str *string) uint64 {
	var res uint64
	res = 1
	for _, r := range *str {
		res = p * res + uint64(r)
	}
	return res
}

func formatCategories(item *gofeed.Item) *string {
	n := len(item.Categories)
	categories := make([]*string, n)
	hashes := make([]uint64, n)
	comp := func(a, b interface{}) int {
		return strings.Compare(*categories[a.(int)], *categories[b.(int)])
	}
	s := treeset.NewWith(comp)
	for i := 0; i < len(item.Categories); i += 1 {
		categories[i] = toHashTag(&item.Categories[i])
		hashes[i] = getHash(categories[i])
	}
	for i := 0; i < len(item.Categories); i += 1 {
		if res, _ := s.Find(func(index int, value interface{}) bool { return hashes[value.(int)] == hashes[i] }); res == -1 {
			s.Add(i)
		}
	}
	values := s.Values()
	res := ""
	for value := 0; value < len(values); value += 1 {
		res += *categories[values[value].(int)] + " "
	}
	return &res
}

func formatItem(feed *gofeed.Feed, itemNumber int) *string {
	item := feed.Items[itemNumber]
	res := "[" + html.EscapeString(feed.Title) + "]\n<b>" +
		html.EscapeString(item.Title) + "</b>\n" +
		html.EscapeString(*formatCategories(item)) + "\n\n" +
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
	res := true
	for i := len(database.Hashes) - 1; i >= 0; i -= 1 {
		if database.Hashes[i] == hash {
			res = false
			break
		}
	}
	if res {
		database.Hashes = append(database.Hashes, hash)
		_, _ = w.WriteString("+ h " + strconv.FormatUint(hash, 10) + "\n")
		_ = w.Sync()
		return true
	}
	return false
}

func update() {
	for i := 0; i < len(database.Urls); i += 1 {
		feeds[i], _ = parser.ParseURL(database.Urls[i])
		for itemNumber := len(feeds[i].Items) - 1; itemNumber >= 0; itemNumber -= 1 {
			if filter(feeds[i].Items[itemNumber]) {
				for idNumber := 0; idNumber < len(database.Ids); idNumber += 1 {
					sendItem(database.Ids[idNumber], feeds[i], itemNumber)
				}
			}
		}
	}
}

func evolve() {
	data, err := ioutil.ReadFile("db.json")
	if err == nil {
		err = json.Unmarshal(data, &database)
	}
	file, err := os.Open("evolution.txt")
	if err != nil {
		log.Panic(err)
	}
	scanner := bufio.NewScanner(file)
	n := len(database.Urls)
	for scanner.Scan() {
		splitted := strings.Split(scanner.Text(), " ")
		if splitted[0] == "+" {
			if splitted[1] == "h" {
				res, _ := strconv.ParseUint(splitted[2], 10, 64)
				database.Hashes = append(database.Hashes, res)
			} else if splitted[1] == "u" {
				database.Urls = append(database.Urls, splitted[2])
				n += 1
			} else if splitted[1] == "i" {
				res, _ := strconv.ParseInt(splitted[2], 10, 64)
				database.Ids = append(database.Ids, res)
			}
		}
	}
	feeds = make([]*gofeed.Feed, n)
	d, err := json.Marshal(database)
	f, err := os.Create("db.json")
	if err != nil {
		log.Panic(err)
	}
	_, _ = f.Write(d)
	defer f.Close()
	_ = f.Sync()
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
						database.Urls = append(database.Urls, args)
						_, _ = w.WriteString("+ u " + args + "\n")
						_ = w.Sync()
						_, _ = bot.Send(tgbotapi.NewMessage(chatId, "Done! New feed index: " + strconv.Itoa(len(feeds) - 1)))
					} else {

					}
				} else {
					msg := tgbotapi.NewMessage(chatId, "Please send me an URL")
					_, _ = bot.Send(msg)
				}
			}
		}
	}
}

func main() {
	proxyURL, err := url.Parse(lookup("HTTP_PROXY"))
	if err != nil {
		log.Panic("Invalid HTTP proxy URL")
	}
	client := http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	bot, err = tgbotapi.NewBotAPIWithClient(lookup("TOKEN"), &client)
	if err != nil {
		log.Panic(err)
	}
	feeds = []*gofeed.Feed{}
	parser = gofeed.NewParser()
	bot.Debug = false
	evolve()
	w, err = os.Create("evolution.txt")
	go updateHandler()
	startPolling()
}
