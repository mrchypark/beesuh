# Beesuh

**대화를 이어 가고, 필요한 메모를 저장하는 Go 개인 비서 런타임입니다.**

Beesuh는 모델 API를 직접 호출하고 대화와 실행 기록을 로컬에 보관합니다.
터미널에서 대화하거나 HTTP API로 연결할 수 있으며, Go 애플리케이션에 런타임을
내장할 수도 있습니다. 기본 `native` 엔진은 별도의 에이전트 서버 없이 실행됩니다.

## 주요 기능

- **이어지는 대화**: 같은 workspace와 session으로 요청하면 최근 성공한 대화를 문맥으로 복원합니다.
- **모델 선택**: OpenAI 호환 Chat Completions, Responses, Anthropic Messages와 Codex CLI를 지원합니다. 이름을 붙인 provider를 요청마다 선택할 수 있습니다.
- **선택적 메모와 스킬**: 대화별 메모를 저장·조회·삭제하고, 설치한 Markdown 스킬을 모델이 읽도록 할 수 있습니다.
- **실행 기록과 중복 요청 처리**: Rhiza에 실행 결과와 대화 기록을 저장합니다. 같은 요청 ID와 입력을 다시 보내면 저장된 결과를 반환합니다.
- **CLI·HTTP·Go 라이브러리**: 개인용 실행에서 시작해, 인증과 사용자별 연결을 갖춘 서비스에 내장할 수 있습니다.

현재 제공하는 것은 런타임과 CLI·HTTP API입니다. 웹 UI나 사용자 로그인 서버,
자동 스케줄러는 포함하지 않습니다.

## 빠르게 시작하기

소스에서 실행하려면 **Go 1.27 이상**, Git, 사용할 모델의 API 주소와 모델 ID가
필요합니다. 메모·스킬 도구를 사용하려면 모델이 tool calling도 지원해야 합니다.

```sh
git clone https://github.com/mrchypark/beesuh.git
cd beesuh

export BEESUH_MODEL_URL='https://model.example/v1'
export BEESUH_MODEL='your-model-id'
export BEESUH_MODEL_API_KEY='your-api-key'

go run ./cmd/beesuh \
  -interactive -workspace personal -session chat-1
```

위 URL·모델 ID·API key는 사용 중인 provider의 값으로 바꿉니다.
`BEESUH_MODEL_URL`에는 Chat Completions API의 base URL을 넣습니다.
클라이언트가 `/chat/completions`를 붙이므로 전체 endpoint를 넣지 않습니다.
인증이 없는 로컬 모델 서버라면 API key 환경 변수를 설정하지 않아도 됩니다.

메시지를 한 줄씩 입력하고 `/quit`으로 종료합니다. 응답은 JSON으로 출력됩니다.
다시 실행할 때 같은 workspace와 session을 사용하면 대화가 이어집니다.
데이터는 기본적으로 현재 디렉터리의 `.beesuh`에 저장됩니다.

한 번만 요청하려면 다음과 같이 실행합니다.

```sh
go run ./cmd/beesuh \
  -workspace personal -session chat-1 -request-id task-1 \
  -message '오늘 할 일을 세 항목으로 정리해 줘.'
```

새 요청에는 새 `request-id`를 사용합니다. `(workspace_id, request_id)`는 중복 요청을
판별하는 키이며, 같은 ID를 다른 입력에 재사용하면 충돌합니다.

## HTTP API로 사용하기

모델 환경 변수는 빠른 시작과 동일합니다. 추가로 운영자용 bearer token을 설정합니다.

```sh
export BEESUH_API_TOKEN='replace-with-a-long-random-private-token'
go run ./cmd/beesuh -listen 127.0.0.1:8080 -operator-http
```

서버가 실행 중인 상태에서 **다른 터미널**에 같은 `BEESUH_API_TOKEN`을 설정한 뒤
요청합니다. 서버와 CLI가 같은 데이터 디렉터리를 동시에 사용해서는 안 됩니다.

```sh
curl http://127.0.0.1:8080/v1/chat \
  -H "Authorization: Bearer $BEESUH_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"workspace_id":"personal","session_key":"chat-1","request_id":"http-1","message":"오늘 할 일을 정리해 줘."}'
```

다음 API도 같은 bearer token을 요구합니다.

| API | 용도 |
| --- | --- |
| `GET /v1/status` | 실행·대기·차단 상태와 backend를 조회합니다. |
| `GET /v1/providers` | provider ID·종류·프로토콜·모델·기본값을 조회합니다. 비밀 설정은 반환하지 않습니다. |
| `GET /v1/history?workspace_id=personal&session_key=chat-1` | native 대화의 최근 journal 기록을 최대 200개 조회합니다. |
| `GET /v1/runs/http-1?workspace_id=personal` | 요청의 실행 상태와 결과를 조회합니다. |

**이 토큰은 신뢰된 운영자·서비스용입니다.** 토큰 보유자는 workspace와 session을
선택하고 기록을 조회할 수 있습니다. 사용자별 권한을 구분하는 로그인 토큰이 아닙니다.
Go 라이브러리를 임베딩하는 다중 사용자 서비스는 검증한 `Principal`을 받는
`httpapi.NewAuthenticated`와 사용자·workspace 권한 검사, 연결별 credential resolver를
구현해야 합니다. 이 모드에서는 `connection_id`가 필수이며 연결 등록은 서비스의
신뢰된 코드에서 수행합니다. 자세한 계약은 [연결 관리](docs/connections-design.md)를 참고하세요.

도커 HTTP 서버는 기존 OAuth2 introspection과 OIDC UserInfo로 사용자를
검증합니다. Goauthy 조회 모드에서는 검증된 사용자마다 개인 workspace를 사용합니다. 모델이 OAuth Bearer를 지원하면
대상 토큰을 검증해 전달하거나 선택적으로 RFC 8693 token exchange를 사용합니다.
외부 서비스에 Beesuh 전용 API를 추가할 필요는 없습니다.
[OAuth2/OIDC 서비스 설정](docs/http-service-adapter.md)을 참고하세요.
Goauthy 연동에는 사용자별 연결 설정 대신 전역 `goauthy.providers` 라우팅만 둡니다.
`GET /v1/connection-options`로 기존 Goauthy API의 내 연결을 조회하고,
기존 Goauthy 화면에서 사용자가 승인한 연결 참조를 `POST /v1/connections`로 선택합니다.
실행할 때 소유자·소비자·resource·scope와 현재 승인을 다시 확인하고, 기존 전달 API에서
키를 한 번만 가져와 메모리에서 사용합니다. 이 키 전달 프로토콜 자체는 OAuth 표준이 아닙니다.
아래 공용 키 예제는 명시적인 `-operator-http` 모드로 실행합니다.

## Docker로 실행하기

Go 설치 없이 실행하려면 `ghcr.io/mrchypark/beesuh` 이미지를 사용합니다.
빠른 시작의 모델 환경 변수와 위의 `BEESUH_API_TOKEN`을 먼저 설정하세요.

```sh
BEESUH_IMAGE=ghcr.io/mrchypark/beesuh:v0.1.0
docker pull --platform linux/amd64 "$BEESUH_IMAGE"
docker run --rm --platform linux/amd64 \
  -p 127.0.0.1:8080:8080 \
  -v beesuh-data:/data \
  -e BEESUH_API_TOKEN -e BEESUH_MODEL_URL \
  -e BEESUH_MODEL -e BEESUH_MODEL_API_KEY \
  "$BEESUH_IMAGE" -listen 0.0.0.0:8080 -data-dir /data -operator-http
```

위 HTTP 요청 예제로 대화할 수 있습니다. 이미지는 Linux amd64용이며,
릴리스를 게시하면 해당 버전 태그의 코드를 CI로 검사한 뒤 같은 버전 태그로만
이미지를 게시합니다. `latest`와 커밋 SHA 태그는 게시하지 않습니다.
기존 버전의 이미지를 게시하려면 `main`에서 CI 워크플로를 수동 실행하고
`release_tag`에 기존 태그(예: `v0.1.0`)를 입력하세요. 모든 검사와 이미지 빌드는
해당 태그의 커밋을 사용하며, 기존 Git 태그는 변경하지 않습니다.

컨테이너는 UID/GID `10001`로 실행합니다. `/data` 볼륨에는 대화와 실행 기록이
남습니다. 호스트 디렉터리를 연결한다면 해당 UID의 쓰기 권한이 필요합니다.
같은 볼륨은 한 인스턴스만 사용하세요.

로컬 빌드는 다음 명령을 사용합니다.

```sh
docker build --platform linux/amd64 -t beesuh:local .
```

이후 실행 예제의 `BEESUH_IMAGE`를 `beesuh:local`로 바꾸면 됩니다.
이미지에는 Codex CLI가 포함되어 있지 않습니다. `codex-cli` provider를 컨테이너에서
사용하려면 Alpine 호환 실행 파일과 로그인 프로파일을 별도로 제공해야 합니다.

## 모델과 계정 설정

단일 Chat Completions 설정은 환경 변수나 `-model-url`, `-model`로 지정합니다.
여러 모델·계정 또는 다른 프로토콜을 사용하려면 JSON provider registry를 지정합니다.

```sh
export OPENCODE_GO_WORK_API_KEY='your-api-key'
go run ./cmd/beesuh \
  -providers examples/providers-go.json -provider go-work \
  -interactive -workspace personal -session chat-1
```

[단일 provider 예제](examples/providers-go.json)와
[다중 provider 예제](examples/providers.json)를 바탕으로 사용 중인 모델에 맞춰 구성하세요.
API key는 JSON에 넣지 않고 `api_key_env`로 환경 변수 이름을 참조합니다.
registry에 등록한 모든 `api_key_env` 값이 필요하며, 예제의 모델 ID와
`codex_home` 경로도 자신의 환경에 맞춰야 합니다.
`-providers`와 `-model-url`·`-model` 플래그는 함께 사용할 수 없습니다.

시작할 때는 선택하지 않은 provider도 모두 검증합니다. 다중 provider 예제의
`codex-cli` 항목을 유지하면 기본 `strict` 토큰 정책으로는 시작할 수 없습니다.
사용하지 않는 Codex 항목을 제거하거나 `-token-budget-policy advisory`를 지정하세요.
`advisory`는 토큰 상한을 보장하지 않으며, `-provider go-work`를 선택해도 registry 전체
검증을 건너뛰지 않습니다.

| provider 종류 | 사용 방식 |
| --- | --- |
| `openai-compatible` | `base_url`과 모델을 지정합니다. 기본은 Chat Completions이며 `api`로 Responses 또는 Anthropic Messages를 선택할 수 있습니다. |
| `opencode-go`, `opencode-zen` | 모델이 요구하는 프로토콜과 API key를 지정합니다. 별도의 OpenCode 서버는 필요하지 않습니다. |
| `codex-api` | OpenAI API key로 공식 Responses API를 호출합니다. |
| `codex-cli` | 설치한 Codex CLI와 로그인된 전용 `codex_home`을 사용합니다. |

`-provider`를 생략하면 registry의 `default`를 사용합니다.
HTTP 요청에서는 `"provider":"go-work"`처럼 선택할 수 있습니다.
같은 session에서 provider를 바꾸면 새 provider에도 기존 대화 문맥이 전달됩니다.
자동 failover나 부하 분산은 제공하지 않습니다. Gemini 프로토콜은 지원하지 않습니다.
전체 필드와 프로토콜 제약은 [provider 설정](docs/providers.md)에 있습니다.

### Codex CLI 사용하기

로그인된 전용 Codex 프로파일의 **절대 경로**를 `codex_home`에 지정합니다.
다음은 자신에게 맞게 바꿔 저장할 registry 구성 예시입니다.

```json
{
  "default": "codex",
  "providers": [
    {
      "id": "codex",
      "kind": "codex-cli",
      "model": "your-codex-model-id",
      "command": "codex",
      "codex_home": "/absolute/path/to/logged-in-codex-home"
    }
  ]
}
```

`providers-codex.json`으로 저장했다면 다음과 같이 실행합니다.

```sh
go run ./cmd/beesuh \
  -providers providers-codex.json -token-budget-policy advisory \
  -interactive -workspace personal -session codex-chat
```

Codex CLI는 출력 토큰의 상한을 강제할 수 없어 기본 `strict` 정책으로는 시작하지
않습니다. `advisory`는 토큰 한도를 힌트로 전달하며 비용 상한을 보장하지 않습니다.
CLI의 내장 도구는 비활성화하고, Beesuh가 도구 호출을 검증·실행합니다.
대화는 Rhiza에 보관하며 Codex CLI 세션을 resume하지 않습니다.

사용자별 프로파일 격리와 실험적 `codex-app-server` 어댑터는
[Codex 프로파일](docs/codex-profiles.md)에 설명되어 있습니다.
실제 계정의 로그인·토큰 갱신 호환성은 별도 검증이 필요합니다.

## 메모·스킬과 실행 옵션

메모와 스킬은 기본적으로 꺼져 있습니다. 모델이 tool calling을 지원한다면
대화 실행 명령에 `-memory` 또는 `-skills-dir /absolute/path/to/skills`를 추가하세요.

- `-memory`: 대화별 메모를 최대 64개 저장합니다. 모델에는 사용자가 명시적으로 요청할 때만 저장하도록 지시합니다.
- `-skills-dir`: 운영자가 설치한 일반 `name.md` 파일을 최대 32개, 파일당 8 KiB까지 읽습니다. 스킬 내용이 실행 권한을 부여하지는 않습니다.

`memory_forget`은 활성 메모만 삭제합니다. 과거 대화 기록과 완료한 실행에는 내용이
남을 수 있으므로 전체 데이터 삭제 기능으로 사용해서는 안 됩니다.

| 옵션 | 기본값 | 용도 |
| --- | --- | --- |
| `-data-dir` | `.beesuh` | 로컬 데이터 디렉터리입니다. |
| `-system` | 빈 값 | 추가 시스템 프롬프트를 지정합니다. |
| `-timeout` | `5m` | 요청의 최대 실행 시간입니다. |
| `-max-concurrent` / `-max-waiting` | `4` / `32` | 동시 실행 수와 대기 요청 수를 제한합니다. |
| `-max-turns` | `12` | 요청당 모델·도구 반복 횟수를 제한합니다. |
| `-max-context-bytes` | `65536` | 복원할 문맥의 크기를 제한합니다. |
| `-max-output-tokens` | `2048` | 모델에 전달할 출력 토큰 한도입니다. |
| `-token-budget-policy` | `strict` | 토큰 상한 정책입니다. `advisory`, `unlimited`도 선택할 수 있습니다. |

전체 옵션은 `go run ./cmd/beesuh -h`로 확인할 수 있습니다.

## 데이터와 현재 제한

Rhiza가 실행 상태·대화 journal·선택적 메모를 로컬에 저장합니다.
모델 API key와 credential 자체는 저장하지 않지만, **사용자 입력과 모델·도구 응답은
기록에 포함됩니다.** 민감한 내용을 입력할 때는 모델 provider와 데이터 디렉터리의
접근 권한·보관 정책을 함께 고려하세요.

같은 session의 요청은 순서대로 실행됩니다. 문맥에는 최근 성공한 8개 turn 중 크기
예산에 맞는 전체 turn만 포함하며 원본 journal은 자동 삭제하지 않습니다.
입력은 12 KiB, 답변은 16 KiB까지이며, 큰 도구 결과는 원문 대신 생략 표시와 digest를 기록합니다.

저장 한도에 도달하면 기존 기록을 지우는 대신 새 실행을 거절합니다.
workspace별 기본 한도는 run 10,000개, session·connection 각각 1,000개이며 전체
디렉터리 한도도 적용됩니다. HTTP에서는 `503 storage_capacity`를 반환합니다.
백업과 디스크 관리는 운영자가 담당합니다.

외부 효과를 다루는 어댑터에서 전송 결과를 확인하지 못하면 `uncertain` 상태로
기록하고 해당 workspace의 새 외부 효과를 차단합니다. 자동 재시도하지 않습니다.
`-recover`는 외부 효과를 되돌리는 명령이 아니므로, 서버를 멈추고 기존 기록과 외부
작업 상태를 확인한 뒤 같은 데이터·도구 설정으로 실행해야 합니다.
자세한 내용은 [실패 복구와 저장 정책](docs/uncertainty-and-retention-policy.md)을 참고하세요.

SSE·스트리밍, 일반 MCP, 동적 도구, 자동 요약·벡터 메모리, 호출별 승인 기능은
현재 제공하지 않습니다. 독립 실행에서 선택할 수 있는 도구는 메모와 정적 스킬 읽기입니다.
기존 OpenCode 서버를 사용하는 `-backend opencode`도 남아 있지만 native 메모와
journal 조회는 지원하지 않으며, 기존 session을 native로 자동 이관하지 않습니다.

## 문서

- [모델 provider 설정](docs/providers.md): registry 형식, 프로토콜, 계정 선택
- [사용자별 연결 관리](docs/connections-design.md): 인증된 서비스에 런타임 내장
- [Codex 프로파일](docs/codex-profiles.md): 계정 격리와 실험적 인증 어댑터
- [API-key 전달 어댑터](docs/goauthy-api-key-delivery.md) · [OAuth 전달 어댑터](docs/goauthy-oauth-delivery.md): 외부 인증 서비스 연동 계약
- [실패 복구와 저장 정책](docs/uncertainty-and-retention-policy.md): 불확실한 실행과 저장 한도
- [통합 검증 범위](docs/local-integration-status.md): 독립 실행과 선택적 연동의 검증 한계

외부 인증 어댑터는 기본 실행의 필수 구성 요소가 아닙니다.
테스트 통과가 실제 provider 계정의 호출·로그인·갱신 검증을 뜻하지는 않습니다.

## 개발과 기여

버그 제보와 개선 제안은 [Issues](https://github.com/mrchypark/beesuh/issues),
변경 제안은 Pull Request로 보내주세요. 재현 명령, 기대한 동작과 실제 동작을
함께 적고, API key·토큰·개인 대화 내용은 제거해 주세요.

소스 변경 후에는 다음 검사를 실행합니다. `GOWORK=off`는 로컬 workspace 대신
`go.mod`에 고정한 의존성을 사용하도록 합니다.

```sh
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go build ./cmd/beesuh
```

Go 코드는 `gofmt`로 정리합니다. OpenCode 플러그인 예제를 수정한다면 Bun이
설치된 환경에서 `(cd examples/opencode && bun test)`도 실행하세요.
CI는 Go 검사, 플러그인 검사, 컨테이너 빌드·실행 검사를 수행합니다.

## 라이선스

Beesuh는 [MIT 라이선스](LICENSE)로 배포됩니다.
