package eventmaster

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"

	eventmaster "github.com/ContextLogic/eventmaster/proto"
)

func getQueryFromRequest(r *http.Request) (*eventmaster.Query, error) {
	var q eventmaster.Query

	// read from request body first - if there's an error, read from query params
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&q); err != nil {
		query := r.URL.Query()
		q.ParentEventId = query["parent_event_id"]
		q.Dc = query["dc"]
		q.Host = query["host"]
		q.TargetHostSet = query["target_host_set"]
		q.User = query["user"]
		q.TagSet = query["tag_set"]
		q.ExcludeTagSet = query["exclude_tag_set"]
		q.TopicName = query["topic_name"]
		if len(query["data"]) > 0 {
			q.Data = query["data"][0]
		}
		if startEventTime := query.Get("start_event_time"); startEventTime != "" {
			q.StartEventTime, _ = strconv.ParseInt(startEventTime, 10, 64)
		}
		if endEventTime := query.Get("end_event_time"); endEventTime != "" {
			q.EndEventTime, _ = strconv.ParseInt(endEventTime, 10, 64)
		}
		if startReceivedTime := query.Get("start_received_time"); startReceivedTime != "" {
			q.StartReceivedTime, _ = strconv.ParseInt(startReceivedTime, 10, 64)
		}
		if endReceivedTime := query.Get("end_received_time"); endReceivedTime != "" {
			q.EndReceivedTime, _ = strconv.ParseInt(endReceivedTime, 10, 64)
		}
		if start := query.Get("start"); start != "" {
			startIndex, _ := strconv.ParseInt(start, 10, 32)
			q.Start = int32(startIndex)
		}
		if limit := query.Get("limit"); limit != "" {
			resultSize, _ := strconv.ParseInt(limit, 10, 32)
			q.Limit = int32(resultSize)
		}
		if tagAndOperator := query.Get("tag_and_operator"); tagAndOperator == "true" {
			q.TagAndOperator = true
		}
		if targetHostAndOperator := query.Get("target_host_and_operator"); targetHostAndOperator == "true" {
			q.TargetHostAndOperator = true
		}
	}
	return &q, nil
}

func wrapHandler(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		defer func() {
			httpReqLatencies.WithLabelValues(r.URL.Path).Observe(trackTime(start))
		}()
		httpReqCounter.WithLabelValues(r.URL.Path).Inc()
		h(w, r, ps)
	}
}

func (s *Server) sendError(w http.ResponseWriter, code int, err error, message string, path string) {
	httpRespCounter.WithLabelValues(path, fmt.Sprintf("%d", code)).Inc()
	errMsg := fmt.Sprintf("%s: %s", message, err.Error())
	fmt.Println(errMsg)
	w.WriteHeader(code)
	w.Write([]byte(errMsg))
}

func (s *Server) sendResp(w http.ResponseWriter, key string, val string, path string) {
	var response []byte
	if key == "" {
		response = []byte(val)
	} else {
		resp := make(map[string]interface{})
		resp[key] = val
		var err error
		response, err = json.Marshal(resp)
		if err != nil {
			s.sendError(w, http.StatusInternalServerError, err, "Error marshalling response to JSON", path)
			return
		}
	}
	httpRespCounter.WithLabelValues(path, "200").Inc()
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (s *Server) handleAddEvent(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var evt UnaddedEvent

	defer r.Body.Close()
	body, err := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(body, &evt); err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error decoding JSON event", r.URL.Path)
		return
	}

	id, err := s.store.AddEvent(&evt)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error writing event", r.URL.Path)
		return
	}
	s.sendResp(w, "event_id", id, r.URL.Path)
}

type EventResult struct {
	EventID       string                 `json:"event_id"`
	ParentEventID string                 `json:"parent_event_id"`
	EventTime     int64                  `json:"event_time"`
	Dc            string                 `json:"dc"`
	TopicName     string                 `json:"topic_name"`
	Tags          []string               `json:"tag_set"`
	Host          string                 `json:"host"`
	TargetHosts   []string               `json:"target_host_set"`
	User          string                 `json:"user"`
	Data          map[string]interface{} `json:"data"`
	ReceivedTime  int64                  `json:"received_time"`
}

type SearchResult struct {
	Results []*EventResult `json:"results"`
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	q, err := getQueryFromRequest(r)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error", r.URL.Path)
		return
	}

	events, err := s.store.Find(q)
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error executing query", r.URL.Path)
		return
	}
	var results []*EventResult
	for _, ev := range events {
		results = append(results, &EventResult{
			EventID:       ev.EventID,
			ParentEventID: ev.ParentEventID,
			EventTime:     ev.EventTime,
			Dc:            s.store.getDcName(ev.DcID),
			TopicName:     s.store.getTopicName(ev.TopicID),
			Tags:          ev.Tags,
			Host:          ev.Host,
			TargetHosts:   ev.TargetHosts,
			User:          ev.User,
			Data:          ev.Data,
		})
	}
	sr := SearchResult{
		Results: results,
	}
	jsonSr, err := json.Marshal(sr)
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error marshalling results into JSON", r.URL.Path)
		return
	}
	s.sendResp(w, "", string(jsonSr), r.URL.Path)
}

func (s *Server) handleGetEventById(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	eventId := ps.ByName("id")
	if eventId != "" {
		ev, err := s.store.FindById(eventId)
		if err != nil {
			s.sendError(w, http.StatusInternalServerError, err, "Error getting event", r.URL.Path)
			return
		}
		result := &EventResult{
			EventID:       ev.EventID,
			ParentEventID: ev.ParentEventID,
			EventTime:     ev.EventTime,
			Dc:            s.store.getDcName(ev.DcID),
			TopicName:     s.store.getTopicName(ev.TopicID),
			Tags:          ev.Tags,
			Host:          ev.Host,
			TargetHosts:   ev.TargetHosts,
			User:          ev.User,
			Data:          ev.Data,
		}
		resultMap := make(map[string]*EventResult)
		resultMap["result"] = result
		bytes, err := json.Marshal(resultMap)
		if err != nil {
			s.sendError(w, http.StatusInternalServerError, err, "Error marshalling response into json", r.URL.Path)
			return
		}
		s.sendResp(w, "", string(bytes), r.URL.Path)
	} else {
		s.sendError(w, http.StatusBadRequest, errors.New("did not provide event id"), "Did not provide event id", r.URL.Path)
	}
}

func (s *Server) handleAddTopic(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	td := Topic{}

	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&td); err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error decoding JSON event", r.URL.Path)
		return
	}

	if td.Name == "" {
		s.sendError(w, http.StatusBadRequest, errors.New("Must include topic_name in request"), "Error adding topic", r.URL.Path)
		return
	}

	id, err := s.store.AddTopic(td)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error adding topic", r.URL.Path)
		return
	}
	s.sendResp(w, "topic_id", id, r.URL.Path)
}

func (s *Server) handleUpdateTopic(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	var td Topic
	defer r.Body.Close()
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error reading request body", r.URL.Path)
		return
	}
	err = json.Unmarshal(reqBody, &td)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error JSON decoding body of request", r.URL.Path)
		return
	}

	topicName := ps.ByName("name")
	if topicName == "" {
		s.sendError(w, http.StatusBadRequest, errors.New("Must provide topic name in URL"), "Error updating topic, no topic name provided", r.URL.Path)
		return
	}
	id, err := s.store.UpdateTopic(topicName, td)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error updating topic", r.URL.Path)
		return
	}
	s.sendResp(w, "topic_id", id, r.URL.Path)
}

func (s *Server) handleGetTopic(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	topics, err := s.store.GetTopics()
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error getting topics from store", r.URL.Path)
		return
	}

	topicSet := make(map[string][]Topic)
	topicSet["results"] = topics
	str, err := json.Marshal(topicSet)
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error marshalling response to JSON", r.URL.Path)
		return
	}
	s.sendResp(w, "", string(str), r.URL.Path)
}

func (s *Server) handleDeleteTopic(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	topicName := ps.ByName("name")
	if topicName == "" {
		s.sendError(w, http.StatusBadRequest, errors.New("Must provide topic name in URL"), "Error deleting topic, no topic name provided", r.URL.Path)
		return
	}
	err := s.store.DeleteTopic(&eventmaster.DeleteTopicRequest{
		TopicName: topicName,
	})
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error deleting topic from store", r.URL.Path)
		return
	}
	s.sendResp(w, "", "", r.URL.Path)
}

func (s *Server) handleAddDc(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var dd Dc
	defer r.Body.Close()
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error reading request body", r.URL.Path)
		return
	}
	err = json.Unmarshal(reqBody, &dd)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error JSON decoding body of request", r.URL.Path)
		return
	}
	id, err := s.store.AddDc(&eventmaster.Dc{
		DcName: dd.Name,
	})
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error adding dc", r.URL.Path)
		return
	}
	s.sendResp(w, "dc_id", id, r.URL.Path)
}

func (s *Server) handleUpdateDc(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	var dd Dc
	defer r.Body.Close()
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error reading request body", r.URL.Path)
		return
	}
	err = json.Unmarshal(reqBody, &dd)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error JSON decoding body of request", r.URL.Path)
		return
	}
	dcName := ps.ByName("name")
	if dcName == "" {
		s.sendError(w, http.StatusBadRequest, err, "Error updating topic, no topic name provided", r.URL.Path)
		return
	}
	id, err := s.store.UpdateDc(&eventmaster.UpdateDcRequest{
		OldName: dcName,
		NewName: dd.Name,
	})
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error updating dc", r.URL.Path)
		return
	}
	s.sendResp(w, "dc_id", id, r.URL.Path)
}

func (s *Server) handleGetDc(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	dcSet := make(map[string][]Dc)
	dcs, err := s.store.GetDcs()
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error getting dcs from store", r.URL.Path)
		return
	}
	dcSet["results"] = dcs
	str, err := json.Marshal(dcSet)
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error marshalling response to JSON", r.URL.Path)
		return
	}
	s.sendResp(w, "", string(str), r.URL.Path)
}

func (s *Server) handleGitHubEvent(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var info map[string]interface{}

	defer r.Body.Close()
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error reading request body", r.URL.Path)
		return
	}
	if err := json.Unmarshal(reqBody, &info); err != nil {
		s.sendError(w, http.StatusBadRequest, err, "Error JSON decoding body of request", r.URL.Path)
		return
	}

	id, err := s.store.AddEvent(&UnaddedEvent{
		Dc:        "github",
		Host:      "github",
		TopicName: "github",
		Data:      info,
	})
	if err != nil {
		s.sendError(w, http.StatusInternalServerError, err, "Error adding event to store", r.URL.Path)
		return
	}
	s.sendResp(w, "event_id", id, r.URL.Path)
}

func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	// TODO: make this more useful
	s.sendResp(w, "", "", r.URL.Path)
}