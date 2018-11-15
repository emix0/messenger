package messenger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	// ProfileURL is the API endpoint used for retrieving profiles.
	// Used in the form: https://graph.facebook.com/v2.6/<USER_ID>?fields=<PROFILE_FIELDS>&access_token=<PAGE_ACCESS_TOKEN>
	ProfileURL = "https://graph.facebook.com/v2.6/"
	// ProfileFields is a list of JSON field names which will be populated by the profile query.
	ProfileFields = "id,name,profile_pic"
	// SendSettingsURL is API endpoint for saving settings.
	SendSettingsURL = "https://graph.facebook.com/v2.6/me/thread_settings"
)

// Options are the settings used when creating a Messenger client.
type Options struct {
	// Verify sets whether or not to be in the "verify" mode. Used for
	// verifying webhooks on the Facebook Developer Portal.
	Verify bool
	// AppSecret is the app secret from the Facebook Developer Portal. Used when
	// in the "verify" mode.
	AppSecret string
	// VerifyToken is the token to be used when verifying the Webhook. Is set
	// when the webhook is created.
	VerifyToken string
	// Token is the access token of the Facebook page to send messages from.
	Token string
	// Client is to allow use of custom clients like default or Google App Engine urlfetcher
	Client *http.Client
}

// Messenger is the client which manages communication with the Messenger Platform API.
type Messenger struct {
	client      *http.Client
	mux         *http.ServeMux
	token       string
	verify      bool
	appSecret   string
	verifyToken string
}

// New creates a new Messenger. You pass in Options in order to affect settings.
func New(mo Options) *Messenger {
	if mo.Client == nil {
		mo.Client = http.DefaultClient
	}

	m := &Messenger{
		client:      mo.Client,
		token:       mo.Token,
		verify:      mo.Verify,
		appSecret:   mo.AppSecret,
		verifyToken: mo.VerifyToken,
	}

	return m
}

// MuxOptions used to initialize options for http handler
type MuxOptions struct {
	// Mux is shared mux between several Messenger objects
	Mux *http.ServeMux
	// WebhookURL is where the Messenger client should listen for webhook events. Leaving the string blank implies a path of "/".
	WebhookURL string
}

// SetupHandler for http handler options
func (m *Messenger) SetupHandler(mo *MuxOptions) {
	if mo.Mux == nil {
		mo.Mux = http.NewServeMux()
	}

	if mo.WebhookURL == "" {
		mo.WebhookURL = "/"
	}

	m.mux = mo.Mux

	m.mux.HandleFunc(mo.WebhookURL, m.Handle)
}

// Handler returns the Messenger in HTTP client form.
func (m *Messenger) Handler() http.Handler {
	if m.mux == nil {
		m.SetupHandler(&MuxOptions{})
	}
	return m.mux
}

// ProfileByID retrieves the Facebook user associated with that ID
func (m *Messenger) ProfileByID(id int64) (Profile, error) {
	p := Profile{}
	url := fmt.Sprintf("%v%v", ProfileURL, id)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return p, err
	}

	req.URL.RawQuery = "fields=" + ProfileFields + "&access_token=" + m.token

	resp, err := m.client.Do(req)
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return p, err
	}

	err = json.Unmarshal(content, &p)
	if err != nil {
		return p, err
	}

	if p == *new(Profile) {
		qr := QueryResponse{}
		err = json.Unmarshal(content, &qr)
		if qr.Error != nil {
			err = fmt.Errorf("Facebook error : %s", qr.Error.Message)
		}
	}

	return p, err
}

// GreetingSetting sends settings for greeting
func (m *Messenger) GreetingSetting(text string) error {
	d := GreetingSetting{
		SettingType: "greeting",
		Greeting: GreetingInfo{
			Text: text,
		},
	}

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", SendSettingsURL, bytes.NewBuffer(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.URL.RawQuery = "access_token=" + m.token

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkFacebookError(resp.Body)
}

// CallToActionsSetting sends settings for Get Started or Persist Menu
func (m *Messenger) CallToActionsSetting(state string, actions []CallToActionsItem) error {
	d := CallToActionsSetting{
		SettingType:   "call_to_actions",
		ThreadState:   state,
		CallToActions: actions,
	}

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", SendSettingsURL, bytes.NewBuffer(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.URL.RawQuery = "access_token=" + m.token

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkFacebookError(resp.Body)
}

// Handle is the internal HTTP handler for the webhooks.
// Exposed to skip initialization if using GAE or similar need
func (m *Messenger) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		m.verifyHandler(w, r)
		return
	}

	var rec Receive

	ctx := r.Context()

	// consume a *copy* of the request body
	body, _ := ioutil.ReadAll(r.Body)
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	err := json.Unmarshal(body, &rec)
	if err != nil {
		fmt.Println("could not decode response:", err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Checks this is an event from a page subscription
	if rec.Object != "page" {
		http.Error(w, "Object is not page, undefined behaviour. Got: "+rec.Object, http.StatusUnprocessableEntity)
		return
	}

	if m.verify {
		if err := m.checkIntegrity(r); err != nil {
			fmt.Println("could not verify request:", err)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}

	m.dispatch(ctx, rec)

	fmt.Fprintln(w, `{status: 'ok'}`)
}

// checkIntegrity checks the integrity of the requests received
func (m *Messenger) checkIntegrity(r *http.Request) error {
	if m.appSecret == "" {
		return fmt.Errorf("missing app secret")
	}

	sigHeader := "X-Hub-Signature"
	sig := strings.SplitN(r.Header.Get(sigHeader), "=", 2)
	if len(sig) == 1 {
		if sig[0] == "" {
			return fmt.Errorf("missing %s header", sigHeader)
		}
		return fmt.Errorf("malformed %s header: %v", sigHeader, strings.Join(sig, "="))
	}

	checkSHA1 := func(body []byte, hash string) error {
		mac := hmac.New(sha1.New, []byte(m.appSecret))
		if mac.Write(body); fmt.Sprintf("%x", mac.Sum(nil)) != hash {
			return fmt.Errorf("invalid signature: %s", hash)
		}
		return nil
	}

	body, _ := ioutil.ReadAll(r.Body)
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	sigEnc := strings.ToLower(sig[0])
	sigHash := strings.ToLower(sig[1])
	switch sigEnc {
	case "sha1":
		return checkSHA1(body, sigHash)
	default:
		return fmt.Errorf("unknown %s header encoding, expected sha1: %s", sigHeader, sig[0])
	}
}

// dispatch triggers all of the relevant handlers when a webhook event is received.
func (m *Messenger) dispatch(ctx context.Context, r Receive) {
	for _, entry := range r.Entry {
		for _, info := range entry.Messaging {
			a := m.classify(info, entry)
			if a == UnknownAction {
				fmt.Println("Unknown action:", info)
				continue
			}

			resp := &Response{
				to:     Recipient{info.Sender.ID},
				token:  m.token,
				client: m.client,
			}

			switch a {
			case TextAction:
				for _, f := range Handlers.messageHandlers {
					message := *info.Message
					message.Sender = info.Sender
					message.Recipient = info.Recipient
					message.Time = time.Unix(info.Timestamp/int64(time.Microsecond), 0)
					f(ctx, message, resp)
				}
			case DeliveryAction:
				for _, f := range Handlers.deliveryHandlers {
					f(ctx, *info.Delivery, resp)
				}
			case ReadAction:
				for _, f := range Handlers.readHandlers {
					f(ctx, *info.Read, resp)
				}
			case PostBackAction:
				for _, f := range Handlers.postBackHandlers {
					message := *info.PostBack
					message.Sender = info.Sender
					message.Recipient = info.Recipient
					message.Time = time.Unix(info.Timestamp/int64(time.Microsecond), 0)
					f(ctx, message, resp)
				}
			case OptInAction:
				for _, f := range Handlers.optInHandlers {
					message := *info.OptIn
					message.Sender = info.Sender
					message.Recipient = info.Recipient
					message.Time = time.Unix(info.Timestamp/int64(time.Microsecond), 0)
					f(ctx, message, resp)
				}
			case ReferralAction:
				for _, f := range Handlers.referralHandlers {
					message := *info.ReferralMessage
					message.Sender = info.Sender
					message.Recipient = info.Recipient
					message.Time = time.Unix(info.Timestamp/int64(time.Microsecond), 0)
					f(ctx, message, resp)
				}
			case AccountLinkingAction:
				for _, f := range Handlers.accountLinkingHandlers {
					message := *info.AccountLinking
					message.Sender = info.Sender
					message.Recipient = info.Recipient
					message.Time = time.Unix(info.Timestamp/int64(time.Microsecond), 0)
					f(ctx, message, resp)
				}
			}
		}
	}
}

// Response returns new Response object
func (m *Messenger) Response(to int64) *Response {
	return &Response{
		to:    Recipient{to},
		token: m.token,
	}
}

// Send will send a textual message to a user. This user must have previously initiated a conversation with the bot.
func (m *Messenger) Send(to Recipient, message string, messagingType MessagingType, tags ...string) error {
	return m.SendWithReplies(to, message, nil, messagingType, tags...)
}

// SendGeneralMessage will send the GenericTemplate message
func (m *Messenger) SendGeneralMessage(to Recipient, elements *[]StructuredMessageElement, messagingType MessagingType, tags ...string) error {
	r := &Response{
		token: m.token,
		to:    to,
	}
	return r.GenericTemplate(elements, messagingType, tags...)
}

// SendWithReplies sends a textual message to a user, but gives them the option of numerous quick response options.
func (m *Messenger) SendWithReplies(to Recipient, message string, replies []QuickReply, messagingType MessagingType, tags ...string) error {
	response := &Response{
		token: m.token,
		to:    to,
	}

	return response.TextWithReplies(message, replies, messagingType, tags...)
}

// Attachment sends an image, sound, video or a regular file to a given recipient.
func (m *Messenger) Attachment(to Recipient, dataType AttachmentType, url string, messagingType MessagingType, tags ...string) error {
	response := &Response{
		token: m.token,
		to:    to,
	}

	return response.Attachment(dataType, url, messagingType, tags...)
}

// classify determines what type of message a webhook event is.
func (m *Messenger) classify(info MessageInfo, e Entry) Action {
	if info.Message != nil {
		return TextAction
	} else if info.Delivery != nil {
		return DeliveryAction
	} else if info.Read != nil {
		return ReadAction
	} else if info.PostBack != nil {
		return PostBackAction
	} else if info.OptIn != nil {
		return OptInAction
	} else if info.ReferralMessage != nil {
		return ReferralAction
	} else if info.AccountLinking != nil {
		return AccountLinkingAction
	}
	return UnknownAction
}

// newVerifyHandler returns a function which can be used to handle webhook verification
func (m *Messenger) verifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("hub.verify_token") == m.verifyToken {
		fmt.Fprintln(w, r.FormValue("hub.challenge"))
		return
	}
	http.Error(w, "Incorrect verify token", http.StatusForbidden)
}
