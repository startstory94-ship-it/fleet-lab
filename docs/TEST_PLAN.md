# 테스트 계획과 리뷰

작성: 2026-11-03. 리뷰는 Codex 사용량 한도(11/4 15:41까지) 때문에 Claude가 직접 했다.

표시: ✅ 있음 · ⬜ 계획 · 🔧 리뷰에서 추가 제안 · ❗ 리뷰에서 찾은 문제

## 테스트 종류

| 종류 | 도구 | 속도 | 언제 |
|---|---|---|---|
| 단위 | `go test`, 가짜 시계 | 1초 안 | 단계마다, 로직마다 |
| 통합 | `go test` + `internal/sim` 가짜 로봇(엘리베이터는 6단계) | 1초 안 | ✅ 3단계부터 |
| 실제 환경 | ROS 2 mock 로봇 + 어댑터 + Mosquitto (수동 → 스크립트화) | 수십 초 | 단계 완료 기준 확인 |
| 부하 | `cmd/loadtest` | 분 단위 | 7단계 |
| 어댑터 | `pytest` (ROS 없이 순수 로직만) | 1초 안 | 🔧 지금부터 |

---

## 1단계: 연결

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 옛날 seq·중복 seq 무시 | ✅ `TestApplyPoseDropsStaleAndDuplicate` | 좋음 |
| 어댑터 재시작(boot 변경) 시 seq 초기화 | ✅ `TestNewBootResetsSeq` | 좋음 |
| pose와 state seq는 따로 셈 | ✅ `TestPoseAndStateSeqAreIndependent` | 좋음 |
| 마지막 수신 시각은 서버 시계, 버린 메시지는 갱신 안 함 | ✅ `TestLastSeenUsesServerClock` | 좋음 |
| Snapshot은 정렬된 복사본 | ✅ `TestSnapshotIsSortedCopy` | 좋음 |
| MQTT 토픽 해석 (`Ingest`): 잘못된 토픽·JSON, 어댑터 상태 | 🔧 없음 | 토픽 파싱에 분기가 많은데 테스트가 없다. 표 테스트로 추가 |
| 재접속 백오프: 간격 증가, 30초 상한, 무작위 범위 | 🔧 없음 | 앞서 1단계 테스트로 설명했지만 실제로는 작성하지 않았다 |
| REST: `/robots/{name}` 없는 로봇 404, `/events?n=` | 🔧 없음 | `httptest`로 간단히 |
| 어댑터: 중복 명령 무시, 만료 명령 거부, 모르는 명령, 위치 데드밴드(5cm·2°), 조용한 로봇은 전달 안 함 | 🔧 없음 | 로직을 ROS와 분리하면 `pytest`로 검사 가능 |
| 실제 환경: 3대 연결, 명령 왕복, 옛날 메시지 주입, LWT | ✅ 수동으로 확인 | ❗ 재현할 수 없다. 스크립트로 만들어 단계마다 다시 돌릴 수 있게 해야 한다 |

## 2단계: 끊김 감지

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 짧은 끊김은 STALE, OFFLINE 아님 | ✅ `TestShortGapIsStaleNotOffline` | 좋음 |
| 20초 끊김은 OFFLINE, 이벤트 순서 STALE→OFFLINE | ✅ `TestLongGapGoesOffline` | 좋음 |
| 복구는 연속 3번, 중간에 끊기면 다시 셈 | ✅ `TestRecoveryNeedsConsecutiveMessages` | STALE에서 복구만 본다. 🔧 OFFLINE에서 복구, 어댑터 재시작 후 복구도 추가 |
| 어댑터 LWT는 그 어댑터의 로봇만 OFFLINE | ✅ `TestAdapterOfflineDropsItsRobotsOnly` | ❗ 아래 문제 2 |
| 예고된 끊김(엘리베이터)은 OFFLINE 아님 | ✅ `TestAnnouncedBlackoutIsNotOffline` | 좋음 |
| 끊김 직전에 온 위치 메시지가 예고를 끝내지 않음 | ✅ `TestPoseDuringBlackoutDoesNotEndIt` | 좋음 |
| 예고 시간 초과 → 경보 1번 → 그 시점부터 다시 셈 | ✅ `TestOverdueBlackoutRaisesAlarmThenTimesOut` | 좋음 |
| 모드·에러 변화 이벤트, 변화 없으면 이벤트 없음 | ✅ `TestModeAndErrorEvents` | 처음 본 로봇도 `mode: →IDLE` 이벤트를 낸다. 🔧 `robot_seen` 이벤트로 구분하는 편이 로그 읽기 쉽다 |
| 느린 수신자는 이벤트를 버리고 다른 수신자는 정상 | ✅ `TestSlowSubscriberDoesNotBlockOthers` | 좋음 |
| 최근 이벤트 보관 | ✅ `TestRecentKeepsLatest` | 개수만 확인한다. 🔧 순서와 내용(가장 최근 것이 남는지)도 확인 |
| 구독 해제 후 전달 안 됨 | ✅ `TestUnsubscribeStopsDelivery` | 좋음 |
| 예고를 반복하면 기한이 늘어남 | 🔧 없음 | 동작은 하지만 의도한 규칙인지 테스트로 못 박아야 한다 |
| 동시에 들어오는 메시지와 Tick의 이벤트 순서 | 🔧 없음 | ❗ 아래 문제 3 |
| WebSocket `/ws` 실제 연결 | 🔧 없음 | `httptest` 서버 + websocket 클라이언트로 한 개 |
| 실제 환경: 로밍, 25초 끊김, 어댑터 종료 | ✅ 수동으로 확인 | 1단계와 같은 문제(재현 불가) |

## 3단계: 작업 배정

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 가장 가까운 로봇, 같은 층 우선 | ✅ `TestNearestRobot` | 다른 층은 층마다 1000 m 벌점, 거리가 같으면 이름순. 가짜 로봇 목록을 일부러 정렬하지 않고 넘겨 이름순 규칙을 확인 |
| 대기열은 먼저 온 순서 | ✅ `TestQueueIsFIFO` | |
| STALE·OFFLINE·ERROR·배터리 부족 로봇 제외 | ✅ `TestEligibility` (표 12줄) | IN_ELEVATOR, 바쁜 모드, IDLE인데 에러, 다른 `task_id` 보고, 이미 작업 보유도 포함 |
| 응답 없으면 같은 `cmd_id` 재전송, 횟수 초과면 실패, 유효 기간 | ✅ `TestResendSameCmdIDThenFail`, `TestGotoCommand` | `expires_at`은 보낼 때마다 새로 계산. 답 없는 로봇을 CHARGING으로 치워 두던 부분을 없앰(리뷰 3) |
| STALE이면 재전송 시간을 세지 않음 | ✅ `TestStaleRobotPausesResends` | |
| `done`이면 다음 구간, 중복 `done`은 두 구간을 넘기지 않음 | ✅ `TestDoneAdvancesLegs` | |
| `failed`/`cancelled`면 재배정, 시도 횟수 초과면 FAILED | ✅ `TestFailedAndCancelledRequeueUntilMaxAttempts` | |
| 옛 `cmd_id`·다른 로봇의 응답 무시 | ✅ `TestStaleAndForeignAcksIgnored` | |
| 로봇 상태의 `task_id`도 `accepted`로 봄 | ✅ `TestRobotReportCountsAsAccepted` | `accepted`가 유실돼도 재배정하지 않아 두 로봇이 같은 작업을 하지 않는다 |
| `done` 유실 → 같은 `cmd_id` 재전송으로 되찾음 | ✅ `TestLostDoneAckIsAskedAgain` | 어댑터가 마지막 응답을 다시 보내는 것(문제 1 수정)에 기댄다 |
| 수행 중 OFFLINE·ERROR면 재배정, STALE·엘리베이터는 유지 | ✅ `TestRobotLoss` | |
| 돌아온 로봇이 옛 작업을 하고 있으면 취소 | ✅ `TestReturnedRobotWithOldTaskIsCancelled` | 취소 응답이 없으면 3초마다 다시 보냄 |
| 사용자 취소는 재배정하지 않음 | ✅ `TestUserCancel` | |
| 이벤트 순서, 바뀔 때마다 저장 | ✅ `TestEventsAndPersistence` | |
| SQLite 저장·읽기 | ✅ `TestRoundTrip`, `TestReopenKeepsData` | |
| 서버 재시작 후 SQLite에서 복구 | ✅ `TestRestartRules`, `TestRestoredLegFinishedWhileDown`, `TestNewIDsContinueAfterRestart`, `TestRestoreGraceStartsAtLinkUp`, `TestRestoreKeepsTaskOfUnreachableRobot` | 유예는 MQTT 첫 연결부터 25초. 위치만 온 로봇은 아직 모름. STALE·엘리베이터 로봇은 OFFLINE까지 기다림. 그 사이 한 번 같은 `cmd_id`로 물어 서버가 꺼진 동안 끝난 구간을 `done`으로 받는다 |
| 손상된 행은 건너뛰고 시작, 그 번호는 다시 쓰지 않음 | ✅ `TestCorruptRowIsSkipped` | |
| REST `/tasks` 네 개, 400·404·409 | ✅ `TestPostTask`, `TestGetTasks`, `TestCancelTask` | |
| 같은 명령 두 번 → 한 번만 실행 | ✅ e2e (어댑터) + `internal/sim` 의 `Started` 검사 | |
| 🔧 작업 완료 판정이 이전 상태 메시지에 속지 않음 | ✅ | 완료는 상태가 아니라 `cmd_id`가 맞는 `done` 응답으로만 판단한다. 어댑터 쪽은 문제 4 수정 |
| 작업 10개를 3대가 나눠 끝냄, 같은 작업을 두 로봇이 동시에 하지 않음 | ✅ `TestTenTasksThreeRobots` (`internal/sim`) | 매 250 ms마다 "한 작업에 로봇 하나" 검사 |
| 수행 중 고장 → 재배정 후 완료 | ✅ `TestFaultMidTaskIsReassigned` | |
| 20초 넘게 조용함 → 재배정, 돌아온 로봇은 취소 | ✅ `TestSilentRobotIsReplacedAndCancelledOnReturn` | |
| 응답 30% 유실에도 모두 완료 | ✅ `TestDroppedAcksStillFinishEverything` | 리뷰 뒤 sim에 지연·순서 뒤바뀜을 넣고 시드 300개×(유실 0%, 30%), 지연 0~1.2초로 따로 돌려 봄: 600번 모두 완료, 겹침 0번 |
| 취소가 goto보다 먼저 도착해도 다시 취소 | ✅ `TestCancelOvertakenByGotoIsCancelledAgain` | 시드 20개 중 11개에서 실제로 순서가 뒤바뀜 |
| sim 겹침 검사가 실제로 잡는지 | ✅ 수동 확인 | 지켜보기 취소를 잠시 끄면 `TestSilentRobot…`과 `TestCancelOvertaken…`이 "driven by robot_1 and robot_2 at once" 등으로 실패함을 확인 |
| 실제 환경: 작업 6개 완료·로봇마다 1개 이상, 수행 중 서버 재시작 후 완료 | ✅ `scripts/e2e.py` 9·10번 (시나리오 24개) | 재시작 뒤 작업이 재배정되지 않고(`dispatches == 1`) 이어지는지도 확인 |

### 3단계 리뷰 (2026-11-03, 모두 수정함)

리뷰어가 1·2·3·5를 실패하는 테스트로 증명했다. 그 테스트는 `internal/task/review_test.go`의 `TestReview*`로 옮겼다(수정 전 5개 모두 실패, 수정 후 통과).

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 높음 | 재시작 직후 위치만 보낸 로봇(Mode `""`)을 보고 지켜보기를 버림. 유예가 프로세스 시작부터 5초 | 상태가 없는 로봇은 "모름"으로 보고 지켜보기 유지. 유예는 MQTT 첫 연결(`LinkUp`)부터, OFFLINE 기준(20초)+5초 이상(25초). STALE·엘리베이터 로봇은 OFFLINE까지 기다림 | `TestReviewRestartOrphanDroppedOnFirstPose`, `TestRestoreGraceStartsAtLinkUp`, `TestRestoreKeepsTaskOfUnreachableRobot` |
| 2 높음 | 로봇이 한 번 다른 `task_id`를 보고하면 지켜보기를 버림(명령이 아직 오는 중일 수 있음) | 마지막 goto/cancel을 보낸 시각 + `CmdTTL` + 5초까지 유지. 안전망: ONLINE 로봇이 자기 것이 아닌 알려진 작업의 `cmd_id`를 3초 넘게 보고하면 취소(3초마다) | `TestReviewCancelAssignedOrphanDropped`, `TestReviewNoResponseOrphanDroppedImmediately`, `TestWatchEndsAfterCmdCanNoLongerLand`, `TestNoCancelForBriefStaleReportAfterDone` |
| 3 중상 | ONLINE인데 답 없는 로봇이 같은 작업을 계속 다시 받아 FAILED, 다른 로봇은 놀고 있음 | 작업마다 실패한 로봇 목록(`failed_on`)을 두고 다른 로봇이 비어 있으면 건너뜀. 답 없어 실패한 로봇은 1분 쉼 | `TestReviewDeafRobotBurnsAllAttempts`, `TestCooldownMakesRobotIneligible`, `TestFailedRobotUsedWhenNoOtherIsFree` |
| 4 중 | 지켜보기 목록이 메모리에만 있음 | 2의 안전망으로 대신함(저장하지 않음). 재시작 뒤 돌아온 로봇이 옛 `cmd_id`를 수행 중이면 취소됨을 확인 | `TestReturningRobotCancelledAfterRestart` |
| 5 중 | 저장 실패를 무시(201 응답, 재시작 뒤 ID·`cmd_id` 재사용 → 어댑터가 옛 응답을 돌려줘 새 작업이 움직이지 않고 DONE) | `Submit`은 저장한 뒤에만 작업을 만들고 실패하면 오류(API 500). `cmd_id`에 서버 실행 ID를 넣음. 상태 변경 저장이 실패하면 남겨 두고 다시 시도(메모리 상태가 기준). 손상된 행 이후 번호도 재사용 안 함(`MaxSeq`) | `TestReviewIDReuseAfterFailedSave`, `TestCmdIDsDifferAcrossBoots`, `TestFlushRetriesFailedWrites`, `TestPostTaskStoreFailure` |
| 6 낮음 | 관리자 잠금 안에서 SQLite 쓰기(busy_timeout 5초) — MQTT 수신이 멈출 수 있음 | 잠금 안에서는 바뀐 작업만 표시하고, 별도 고루틴(`RunWriter`)이 작업마다 최신 상태를 순서대로 씀. 종료 때 한 번 더 씀 | `TestFlushRetriesFailedWrites`, 저장 확인 테스트들(`Flush` 사용) |
| 7 낮음 | 시간 제한이 없어 RUNNING이 영원히 갈 수 있음 | 구간마다 거리/0.3 m/s + 30초. 넘으면 로봇 취소 후 재배정. STALE·엘리베이터 시간은 빼고 셈 | `TestLegTimeout` |
| 8 낮음 | API가 JSON 뒤 데이터를 받아 줌, 큰 본문에 400 | 뒤에 데이터가 있으면 400, `http.MaxBytesReader`로 1 MB 넘으면 413 | `TestPostTaskBodyLimits` |
| 테스트 | sim에 지연이 없어 "두 로봇 금지" 검사가 실패할 수 없음 | sim에 명령·응답 지연과 무작위 지연(순서 뒤바뀜 가능), 시작할 때 위치가 상태보다 먼저. 겹침 검사를 250 ms 틱 사이 50 ms마다, 조용한 로봇 테스트에서도 처음부터(링크가 끊긴 로봇과 돌아온 지 2초 안의 로봇은 서버가 멈출 수 없으므로 제외) | `TestCancelOvertakenByGotoIsCancelledAgain`, 기존 sim 테스트 4개 |
| 테스트 | 만료(`expired`) 응답 | 시도 1번을 쓰고 다른 로봇이 끝냄, 취소는 보내지 않음 | `TestExpiredAckUsesAnAttempt` |

## 4단계: 배터리 (계획)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 25% 미만 배정 제외, 하던 일 마친 뒤 충전 | ⬜ | 좋음 |
| 🔧 25% 근처에서 들어갔다 나왔다 반복하지 않음 | 🔧 | 들어가는 기준(25%)과 다시 배정하는 기준(예: 90%)을 다르게 두고 테스트 |
| 🔧 모든 로봇이 충전 중일 때 작업은 대기 | 🔧 | 작업이 사라지거나 실패 처리되면 안 된다 |

## 5단계: 대시보드 (계획)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 묶음에 바뀐 로봇만 포함 | ⬜ | 좋음 |
| 느린 브라우저는 오래된 묶음 버림 | ⬜ | 2단계 이벤트 허브 테스트와 같은 방식 |
| 🔧 묶음은 한 번만 만들어 모든 브라우저에 같은 바이트 전송 | 🔧 | 부하 테스트 비교 항목이라 단위 테스트로도 고정 |
| 화면 보간 | ⬜ | Go로는 시험 불가. 수동 확인 + 데모 GIF |

## 6단계: 목적층이 다른 로봇 함께 태우기 (계획) ⭐

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 3·7·5층 → 탑승 7·5·3, 3층 로봇이 문 쪽 | ⬜ | 좋음 |
| 같은 층 2대, 칸 부족 분할, 방향 다름 분리, 내려갈 때 반대 순서, 0·1대 | ⬜ | 좋음 |
| 🔧 무작위 입력 1,000개로 규칙 검사 | 🔧 | 예시 몇 개보다 강하다. 규칙: ① 모든 정차 층에서 내릴 로봇이 문 쪽에 있다 ② 칸 수를 넘지 않는다 ③ 방향이 섞이지 않는다 ④ 같은 입력이면 같은 결과 |
| 🔧 목적층이 현재 층과 같은 로봇 | 🔧 | 태우지 않아야 한다 |
| 한 대씩 하차, 앞 로봇이 문 구역을 벗어난 뒤 다음 출발 | ⬜ | 가짜 시계로 시간 흐름을 제어해야 빠르고 안정적이다 |
| 하차 실패, 문 앞 막힘, 엘리베이터 응답 없음 | ⬜ | 좋음 |
| 탑승 중 끊김이 OFFLINE으로 잡히지 않음 | ✅ 서버 로직만 (2단계) | 6단계에서 실제 로봇 동작과 이어서 다시 확인 |
| 다시 연결 후 쌓인 이벤트로 시간순 정리, 잘못된 층 하차 감지 | ⬜ | 좋음 |
| 🔧 엘리베이터 벤더 어댑터 2종이 같은 인터페이스로 같은 결과 | 🔧 | 같은 테스트를 두 구현에 돌리는 방식 |

## 7단계: 마무리 (계획)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| CI에서 `go test -race` | ⬜ | 이 PC는 C 컴파일러가 없어 `-race`를 못 돌린다. CI가 유일한 검사 수단 |
| 부하 3종 비교 | ⬜ | 🔧 한 번만 재면 편차가 크다. 3~5번 재서 중앙값을 쓴다 |
| 🔧 CI에서 Mosquitto 띄워 통합 테스트 | 🔧 | GitHub Actions 서비스 컨테이너로 가능 |

---

## 리뷰에서 찾은 문제

> **2026-11-03 모두 수정함.** 1·4·5는 어댑터(`adapter/fleet_logic.py`로 순수 로직 분리, `pytest` 22개),
> 2·3은 서버(`pose`에 `host`, 이벤트를 잠금 안에서 번호 매겨 전송), 6은 `scripts/e2e.bat`(시나리오 17개).
> 위 표의 🔧 단위 테스트(백오프, Ingest, API, WebSocket, OFFLINE 복구, 예고 연장, 이벤트 보관 순서)도 추가했다.

### ❗ 1. 중복 명령에 응답을 다시 보내지 않음 (어댑터) — 높음, 3단계 전에 고쳐야 함
`handle_cmd`는 이미 본 `cmd_id`면 아무 응답 없이 끝난다.
3단계에서 서버가 "응답이 없으면 같은 `cmd_id`로 재전송"하면, 첫 응답이 유실된 경우 로봇은 명령을 실행 중인데
서버는 끝까지 응답을 못 받아 **실패로 처리하고 다른 로봇에게 재배정한다.** 같은 작업을 두 로봇이 하게 된다.
- 고칠 방법: 처리한 `cmd_id`별로 마지막 응답을 기억해 두고, 중복이 오면 그 응답을 다시 보낸다.
- 테스트: 같은 `cmd_id` 두 번 → 실행 1번, 응답 2번(같은 내용).

### ❗ 2. 위치만 먼저 온 로봇은 어댑터 종료를 못 잡음 — 낮음
어댑터 정보(`host`)는 state 메시지에만 있다. 서버가 켜진 직후 pose만 받고 state를 아직 못 받은 로봇은
그 사이 어댑터가 죽어도 OFFLINE이 되지 않는다(20초 타임아웃으로는 잡힘).
- 고칠 방법: pose에도 `host`를 넣거나, 토픽에서 알 수 있게 한다.

### ❗ 3. 이벤트 순서가 뒤바뀔 수 있음 — 중간
상태는 잠금 안에서 바꾸고 이벤트는 잠금 밖에서 내보낸다. Tick과 메시지 처리가 동시에 일어나면
"OFFLINE → ONLINE"이 "ONLINE, OFFLINE" 순서로 대시보드에 도착할 수 있다. 화면이 틀린 상태로 남는다.
- 고칠 방법: 이벤트에 증가하는 번호를 붙이거나, 잠금 안에서 내보낸다(허브가 막히지 않으므로 가능).
- 테스트: 여러 goroutine으로 동시에 메시지와 Tick → 이벤트 번호가 순서대로인지 `-race`와 함께 확인.

### ❗ 4. mock 로봇이 `task_id`를 보내지 않음 — 높음, 3단계 전제
서버와 어댑터는 `task_id`를 주고받게 되어 있지만 mock 로봇은 이 값을 만들지 않는다.
이대로면 3단계에서 "작업 완료"를 `MOVING → IDLE`로만 판단하게 되는데,
명령 직후 도착한 **명령 이전의 IDLE 상태 메시지**를 완료로 오해할 수 있다.
- 고칠 방법: `goto`에 `cmd_id`(또는 `task_id`)를 실어 보내고, 로봇이 상태에 "지금 수행 중인 명령 ID"와 "마지막으로 끝낸 명령 ID"를 넣는다.
- 테스트: 명령 직후 옛 IDLE 상태가 와도 완료로 보지 않음.

### ❗ 5. 서비스 호출 실패 시 어댑터 콜백 예외 — 낮음
`add_done_callback`의 람다가 `f.result()`를 바로 부른다. 서비스 호출이 예외로 끝나면 콜백 안에서 예외가 나고
응답(`cmd_ack`)을 보내지 못한다. 서버는 응답을 기다리다 시간 초과가 난다.
- 고칠 방법: `f.exception()`을 먼저 확인해 `failed`로 응답한다.

### ❗ 6. 실제 환경 테스트가 재현되지 않음 — 중간
1·2단계의 끝단 확인은 손으로 명령을 친 것이라 다음 단계에서 같은 확인을 다시 하기 어렵다.
단계가 쌓이면 예전 기능이 깨져도 모른다.
- 고칠 방법: 시나리오 스크립트(로봇·어댑터·서버 띄우기 → 명령 → 기대 결과 확인 → 정리)를 만들고 단계마다 추가한다.
  ROS 없이 돌릴 수 있는 부분은 `internal/sim` 통합 테스트로 옮긴다.

## 우선순위

1. **3단계 시작 전:** 문제 1, 4 (작업 배정이 올바르게 동작하기 위한 전제)
2. **지금 바로 (작음):** 문제 5, 백오프·Ingest·API 단위 테스트, 이벤트 보관 테스트 보강
3. **3단계와 함께:** 문제 3, 6
4. **나중에:** 문제 2

### 3단계 확인 중 추가로 찾은 문제 (어댑터)
- `goto` 직후 로봇이 다음 상태(2 Hz)를 보내기 전에 `cancel`이 도착하면, 어댑터가 목표 시작을 못 봐서 3초 뒤 `failed: robot did not take the goal`로 끝냈다.
  e2e "cancelled goto reported"가 간헐적으로 실패해 발견했다. `cancel`·`dock` 서비스가 성공하면 추적 중인 `goto`를 바로 `cancelled`로 끝내도록 고쳤다(`GoalTracker.cancel`, pytest 2개 추가).
