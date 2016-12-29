#include "../include/client.h"

#include <chrono>
#include <ctime>
#include <iostream>
#include <math.h>
#include <sstream>
#include <string>
#include <typeinfo>

int messages_processed(0);

char *getTime() {
  using std::chrono::system_clock;

  system_clock::time_point today = system_clock::now();
  std::time_t tt;
  tt = system_clock::to_time_t(today);
  return ctime(&tt);
}

static std::unique_ptr<header_t> ParseHeader(message_t *parsed_message) {
  auto header =
      flatbuf::header::GetHeader(parsed_message->header.c_str());

  std::unique_ptr<header_t> parsed_header(new header_t);
  parsed_header->event = header->event();
  parsed_header->opcode = header->opcode();

  // if (header->metadata() != nullptr) {
    parsed_header->metadata = header->metadata()->str();
  // }

  return parsed_header;
}

void AppWorker::RouteMessageWithoutResponse(header_t *parsed_header,
                                            message_t *parsed_message) {
  std::string key, val;

  auto payload =
      flatbuf::payload::GetPayload((const void *)parsed_message->payload.c_str());
  key.assign(payload->key()->str());
  val.assign(payload->value()->str());

  // cerror_out << "key: " << key << " val: " << val << '\n';
  switch (getEvent(parsed_header->event)) {
  case eDCP:
    switch (getDCPOpcode(parsed_header->opcode)) {
    case oDelete:
      break;
    case oMutation:
      // cerror_out << "header:" << parsed_message->header
      //            << " payload:" << parsed_message->payload
      //            << " val:" << val << '\n';
      this->v8worker->SendUpdate(val, parsed_header->metadata, "json");
      break;
    case DCP_Opcode_Unknown:
      cerror_out << getTime() << "dcp_opcode_unknown encountered" << '\n';
      break;
    }
    break;
  default:
    cerror_out << getTime() << "command:" << parsed_header->event
               << " sent to RouteMessageWithoutResponse and it isn't desirable"
               << '\n';
  }
}

std::string AppWorker::RouteMessageWithResponse(header_t *parsed_header,
                                                message_t *parsed_message) {
  std::string result;

  switch (getEvent(parsed_header->event)) {
  case eV8_Worker:
    switch (getV8WorkerOpcode(parsed_header->opcode)) {
    case oDispose:
    case oInit:
      cerror_out << "Loading app:" << parsed_header->metadata << '\n';
      this->v8worker = new V8Worker(parsed_header->metadata);
      result.assign("Loaded requested app\n");
      return result;
      break;
    case oLoad:
      cerror_out << "Loading app code:" << parsed_header->metadata << '\n';
      this->v8worker->V8WorkerLoad(parsed_header->metadata);
      result.assign("Loaded app code\n");
      return result;
      break;
    case oTerminate:
    case oVersion:
    case V8_Worker_Opcode_Unknown:
      cerror_out << getTime() << "worker_opcode_unknown encountered"
                 << '\n';
      break;
    }
    break;
  case eDCP:
    switch (getDCPOpcode(parsed_header->opcode)) {
    case oDelete:
    case oMutation:
    case DCP_Opcode_Unknown:
      cerror_out << getTime() << "dcp_opcode_unknown encountered" << '\n';
      break;
    }
    break;
  case eHTTP:
    switch (getHTTPOpcode(parsed_header->opcode)) {
    case oGet:
    case oPost:
    case HTTP_Opcode_Unknown:
      cerror_out << getTime() << "http_opcode_unknown encountered" << '\n';
      break;
    }
    break;
  case eV8_Debug:
    switch (getV8DebugOpcode(parsed_header->opcode)) {
    case oBacktrace:
    case oClear_Breakpoint:
    case oContinue:
    case oEvaluate:
    case oFrame:
    case oList_Breakpoints:
    case oLookup:
    case oSet_Breakpoint:
    case oSource:
    case oStart_Debugger:
    case oStop_Debugger:
    case V8_Debug_Opcode_Unknown:
      cerror_out << getTime() << "v8_debug_opcode_unknown encountered"
                 << '\n';
      break;
    }
    break;
  case Event_Unknown:
    cinfo_out << "Unknown command" << '\n';
    break;
  }

  return result;
}

static std::unique_ptr<message_t>
ParseServerMessage(int encoded_header_size, int encoded_payload_size,
                   const std::string &message) {
  std::unique_ptr<message_t> parsed_message(new message_t);
  parsed_message->header = message.substr(
      HEADER_FRAGMENT_SIZE + PAYLOAD_FRAGMENT_SIZE, encoded_header_size);
  parsed_message->payload = message.substr(
      HEADER_FRAGMENT_SIZE + PAYLOAD_FRAGMENT_SIZE + encoded_header_size,
      encoded_payload_size);

  messages_processed++;

  return parsed_message;
}

static void alloc_buffer(uv_handle_t *handle, size_t suggested_size,
                         uv_buf_t *buf) {
  std::vector<char>* read_buffer = AppWorker::GetAppWorker()->GetReadBuffer();
  *buf = uv_buf_init(read_buffer->data(), read_buffer->capacity());
}

AppWorker::AppWorker()
    : main_loop_running(false), conn_handle(nullptr) {
  uv_loop_init(&main_loop);
  read_buffer.resize(MAX_BUF_SIZE);
}

AppWorker::~AppWorker() { uv_loop_close(&main_loop); }

void AppWorker::Init(const std::string &appname, const std::string &addr,
                     int port) {
  uv_tcp_init(&main_loop, &tcp_sock);
  uv_ip4_addr(addr.c_str(), port, &server_sock);

  this->app_name = appname;
  cerror_out << "Starting worker for appname:" << appname << " port:" << port
             << '\n';

  uv_tcp_connect(&conn, &tcp_sock, (const struct sockaddr *)&server_sock,
                 [](uv_connect_t *conn, int status) {
                   AppWorker::GetAppWorker()->OnConnect(conn, status);
                 });

  if (main_loop_running == false) {
    uv_run(&main_loop, UV_RUN_DEFAULT);
    main_loop_running = true;
  }
}

void AppWorker::OnConnect(uv_connect_t *conn, int status) {
  if (status == 0) {
    cerror_out << "Client connected" << '\n';

    uv_read_start(conn->handle, alloc_buffer,
                  [](uv_stream_t *stream, ssize_t nread, const uv_buf_t *buf) {
                    AppWorker::GetAppWorker()->OnRead(stream, nread, buf);
                  });

    conn_handle = conn->handle;
  } else {
    cerror_out << getTime()
               << "Connection failed with error:" << uv_strerror(status)
               << '\n';
  }
}

int combineAsciiToInt(std::vector<int> *input) {
  int result = 0;
  for (int i = 0; i < input->size(); i++) {
    if ((*input)[i] < 0) {
      result = result + pow(256, i) * (256 + (*input)[i]);
    } else {
      result = result + pow(256, i) * (*input)[i];
    }
  }
  return result;
}

void AppWorker::ParseValidChunk(uv_stream_t *stream, int nread,
                                const char *buf) {
  std::string buf_base;
  for (int i = 0; i < nread; i++) {
    buf_base += buf[i];
  }

  if (next_message.length() > 0) {
    buf_base = next_message + buf_base;
    next_message.clear();
  }

  for (; buf_base.length() > HEADER_FRAGMENT_SIZE + PAYLOAD_FRAGMENT_SIZE;) {
    std::vector<int> header_entries, payload_entries;
    int encoded_header_size, encoded_payload_size;

    for (int i = 0; i < HEADER_FRAGMENT_SIZE; i++) {
      header_entries.push_back(int(buf_base[i]));
    }
    encoded_header_size = combineAsciiToInt(&header_entries);

    for (int i = HEADER_FRAGMENT_SIZE;
         i < HEADER_FRAGMENT_SIZE + PAYLOAD_FRAGMENT_SIZE; i++) {
      payload_entries.push_back(int(buf_base[i]));
    }
    encoded_payload_size = combineAsciiToInt(&payload_entries);

    int message_size = HEADER_FRAGMENT_SIZE + PAYLOAD_FRAGMENT_SIZE +
                       encoded_header_size + encoded_payload_size;

    if (buf_base.length() < message_size) {
      next_message.assign(buf_base);
      return;
    } else {
      std::string chunk_to_parse = buf_base.substr(0, message_size);

      std::unique_ptr<message_t> parsed_message = ParseServerMessage(
          encoded_header_size, encoded_payload_size, chunk_to_parse);
      cerror_out << "header_size:" << encoded_header_size << " payload_size "
                 << encoded_payload_size
                 << " messages processed: " << messages_processed << '\n';

      if (parsed_message) {
        std::unique_ptr<header_t> parsed_header =
            ParseHeader(parsed_message.get());

        if (parsed_header) {
          header_t *pheader = parsed_header.get();
          switch (getEvent(pheader->event)) {
          case eDCP:
            RouteMessageWithoutResponse(parsed_header.get(),
                                        parsed_message.get());
            break;
          default:
            std::string result = RouteMessageWithResponse(parsed_header.get(),
                                                          parsed_message.get());

            if (!result.empty()) {
              // TODO: replace it with unique_ptr
              write_req_t *req = new (write_req_t);
              std::string response_buf(result);
              req->buf = uv_buf_init((char *)response_buf.c_str(),
                                     response_buf.length());
              uv_write((uv_write_t *)req, stream, &req->buf, 1,
                       [](uv_write_t *req, int status) {
                         AppWorker::GetAppWorker()->OnWrite(req, status);
                       });
            }
          }
        }
      }
    }
    buf_base.erase(0, message_size);
  }

  if (buf_base.length() > 0) {
      next_message.assign(buf_base);
  }
}

void AppWorker::OnRead(uv_stream_t *stream, ssize_t nread,
                       const uv_buf_t *buf) {
  cerror_out << "OnRead callback triggered, nread: " << nread << '\n';
  if (nread > 0) {
    AppWorker::GetAppWorker()->ParseValidChunk(stream, nread, buf->base);
  } else if (nread == 0) {
    next_message.clear();
  } else {
    if (nread != UV_EOF) {
      cerror_out << getTime() << "Read error, err code: " << uv_err_name(nread)
                 << '\n';
    }
    AppWorker::GetAppWorker()->ParseValidChunk(stream, next_message.length(),
                                               next_message.c_str());
    next_message.clear();
    uv_read_stop(stream);
  }
}

void AppWorker::WriteMessage(Message *msg) {
  uv_write(outgoing_queue.write_bufs.GetNewWriteBuf(), conn_handle,
           msg->GetBuf(), 1, [](uv_write_t *req, int status) {
             AppWorker::GetAppWorker()->OnWrite(req, status);
           });
}

void AppWorker::OnWrite(uv_write_t *req, int status) {
  if (status) {
    cerror_out << getTime() << "Write error, err: " << uv_strerror(status)
               << '\n';
  }

  write_req_t *wr = (write_req_t *) req;
  delete wr;
}

std::vector<char> *AppWorker::GetReadBuffer() { return &read_buffer; }

AppWorker *AppWorker::GetAppWorker() {
  static AppWorker worker;
  return &worker;
}

int main(int argc, char **argv) {
  std::string appname(argv[1]);
  std::string timestamp(argv[3]);
  std::string error_log_file =
      std::string("client.error_") + appname + timestamp + std::string(".log");

  cerror_out.open(error_log_file.c_str());

  AppWorker *worker = AppWorker::GetAppWorker();
  int port = atoi(argv[2]);
  worker->Init("credit_score", "127.0.0.1", port);

  cerror_out.close();
}
