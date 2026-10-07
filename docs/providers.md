# 모델 provider registry

Native backend는 하나의 direct model 설정 또는 이름 있는 registry를 사용한다.
registry를 쓰면 request마다 provider alias를 명시할 수 있다. 같은 kind를 여러 번
넣어도 되며 ID만 고유하면 된다. Beesuh는 provider를 자동 선택하거나 교체하지 않는다.

## 파일 형식

provider 파일은 최대 64 KiB JSON object다. provider는 1~64개이고 ID는 영문·숫자·
underscore·hyphen만 쓴다. unknown field, duplicate ID, 없는 default는 startup error다.
API key 문자열은 파일에 넣지 않는다. api_key_env는 환경 변수 이름이며, 참조한 변수가
비어 있으면 startup을 거부한다. 환경은 deployment shell, service manager, secret
manager 등 파일 밖에서 provision한다.

~~~json
{
  "default": "go-personal",
  "providers": [
    {
      "id": "go-personal",
      "kind": "opencode-go",
      "model": "glm-5.1",
      "api_key_env": "OPENCODE_GO_PERSONAL_API_KEY"
    }
  ]
}
~~~

전체 예시는 [examples/providers.json](../examples/providers.json)에 있다. 그 안의
codex-api-example model ID는 형식 설명용 예시이므로 operator는 자신의 계정에서
사용 가능한 공식 model ID로 바꿔야 한다. API key와 authorization path는 Providers
API와 journal metadata에 노출하지 않는다.

## kind와 API protocol

| kind | API | base_url | 인증/특성 |
| --- | --- | --- | --- |
| openai-compatible | chat-completions 기본값, responses 또는 anthropic 가능 | 필수 | API key는 선택적 bearer token이다. 해당 provider/model이 선택한 protocol과 tool calling을 지원해야 한다. |
| opencode-go | chat-completions 기본값, responses 또는 anthropic 가능 | 생략 시 https://opencode.ai/zen/go/v1 | api_key_env가 일반적이다. Beesuh는 User-Agent beesuh/0.1과 x-opencode-session을 보낸다. |
| opencode-zen | chat-completions 기본값, responses 또는 anthropic 가능 | 생략 시 https://opencode.ai/zen/v1 | Zen API key를 api_key_env로 참조한다. x-opencode-session을 보낸다. |
| codex-api | responses만 | 생략 시 https://api.openai.com/v1 | OpenAI API key가 필요하다. 공식 Responses API를 쓴다. ChatGPT 내부 backend를 역공학하거나 사용하지 않는다. |
| codex-cli | CLI bridge | 사용하지 않음 | 설치된 codex의 login을 쓴다. command 기본값은 codex, codex_home은 선택적인 absolute path다. |

API 값은 chat-completions, responses, anthropic뿐이다. OpenCode Go의 model별 API는
동일하지 않다. 예를 들어 공식 Go 목록에서 GLM-5.1은 chat completions, MiniMax M3는
Anthropic Messages, 일부 model은 Responses를 사용한다. Zen도 model마다 wire가 다르며
Gemini wire는 아직 구현하지 않았다. 모든 Go/Zen model이 OpenAI compatible이라고
가정하지 말고 model별 endpoint를 확인한다.

OpenCode Go가 요구하는 식별 header와 session cache header는
[OpenCode Go 문서](https://opencode.ai/docs/go)에 명시되어 있으며 구현은 이 요구를
따른다. Zen의 gpt-5.3-codex Responses endpoint도 [공식 Zen 문서](https://opencode.ai/docs/zen/)
에서 확인했다. 공식 [Codex non-interactive 문서](https://developers.openai.com/codex/noninteractive/)
및 [OpenAI function calling 가이드](https://developers.openai.com/api/docs/guides/function-calling)는
Codex CLI bridge와 Codex API tool call 경계를 설명하는 근거다.

## 선택

~~~sh
go run ./cmd/beesuh -providers examples/providers.json -provider go-work \
  -interactive -workspace personal -session chat-1

go run ./cmd/beesuh -providers examples/providers.json -provider go-work \
  -workspace personal -session chat-1 -request-id first \
  -message '오늘 일을 정리해 줘.'
~~~

-provider를 생략하면 registry default가 쓰인다. HTTP direct chat JSON에는
optional provider field를 넣는다.

~~~json
{
  "workspace_id": "personal",
  "session_key": "chat-1",
  "request_id": "first",
  "provider": "go-work",
  "message": "오늘 일을 정리해 줘."
}
~~~

GET /v1/providers는 ID, kind, API, model, default만 반환한다. base URL, API key,
api_key_env, CodexHome은 반환하지 않는다.

## session, credentials, limits

같은 session history는 선택한 provider로 의도적으로 이어진다. alias를 바꾸면 새
provider가 기존 message history를 읽고 다음 turn을 만든다. OpenAI Responses의 opaque
provider state/reasoning은 같은 process, 같은 alias, 같은 effective config에서만 다시
보낸다. restart 또는 alias/config 변경 시 opaque state를 버리고 portable message history로
재구성한다. API key는 DB에 hash조차 저장하지 않으며 Codex login data도 provider 간에
공유하지 않는다.

registry는 explicit alias 선택만 제공하며 automatic failover, load balancing, retry는 없다. request
실패는 해당 request의 실패다. provider별 quota, price, availability와 model compatibility는
operator가 관리한다. 실제 credential을 사용한 provider 호출은 이 저장소에서 검증하지
않았다.

codex-cli는 설치된 Codex login을 CodexHome별로 사용한다. CodexHome을 주면 absolute
directory여야 하며, user config/rules는 무시한다. CLI bridge는 temporary work directory와
read-only sandbox를 사용하고 shell, browser, MCP, apps, plugins, hooks, multi-agent를
끄며, typed JSON만 Beesuh로 돌려준다. tool 실행은 Beesuh가 한다. CLI session resume은
없고 Rhiza가 conversation을 보존하며 각 model call은 ephemeral이다. max_output_tokens는
CLI에 hint로만 전달되며 provider-side hard token limit은 아니다.


`codex-app-server`는 명시 선택하는 실험적 external ChatGPT auth consumer다.
GoAuthy에서 연결한 여러 계정 중 선택된 profile을 격리하는 방식과 token-source 계약은
[Codex 프로파일](codex-profiles.md)을 참고한다.
