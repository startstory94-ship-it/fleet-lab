# 테스트 계획과 리뷰

작성: 2026-11-03. 리뷰는 Codex 사용량 한도(11/4 15:41까지) 때문에 Claude가 직접 했다.

표시: ✅ 있음 · ⬜ 계획 · 🔧 리뷰에서 추가 제안 · ❗ 리뷰에서 찾은 문제

## 테스트 종류

| 종류 | 도구 | 속도 | 언제 |
|---|---|---|---|
| 단위 | `go test`, 가짜 시계 | 1초 안 | 단계마다, 로직마다 |
| 통합 | `go test` + `internal/sim` 가짜 로봇(엘리베이터는 6단계) | 1초 안 | ✅ 3단계부터 |
| 실제 환경 | ROS 2 mock 로봇 + 어댑터 + Mosquitto (수동 → 스크립트화) | 수십 초 | 단계 완료 기준 확인 |
| 부하 | `cmd/loadtest` (실제 fleetd + Mosquitto) | 약 9분 | ✅ 7단계, [LOAD_TEST.md](LOAD_TEST.md) |
| 어댑터 | `pytest` (ROS 없이: 순수 로직 + 가짜 rclpy로 배선) | 1초 안 | ✅ 1단계 리뷰부터, CI |

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

## 4단계: 배터리

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 25% 미만 배정 제외, 하던 일 마친 뒤 충전 | ✅ `TestBusyRobotDocksAfterItsTask`, `TestEligibility` | 구간 사이(다음 구간 ASSIGNED)에도 도크를 보내지 않음을 확인 |
| 25% 근처에서 들어갔다 나왔다 반복하지 않음 | ✅ `TestBatteryHysteresis` (표 10줄) | 24/25/26, 표시 뒤 26·60·89는 여전히 제외, 90에서 해제, 24↔26 반복해도 이벤트 `low` 한 번 |
| `dock` 한 번 → 10초 뒤 새 `cmd_id`로 재전송 → 3번 뒤 `dock_failed`, 1분 쉼 | ✅ `TestDockSentResentThenBacksOff` | `expires_at`, 쉬는 동안 0번, `DOCKING`/`CHARGING` 중 0번, 충전 뒤 배정까지 이벤트 순서 |
| 충전이 중간에 끊긴 로봇은 다시 도크로 | ✅ `TestDockAgainAfterChargingStoppedEarly` | |
| STALE·OFFLINE·IN_ELEVATOR·ERROR·IDLE인데 에러·이미 DOCKING/CHARGING·작업 없는 goto 수행 중·IDLE인데 `task_id` 보고·거둔 goto를 지켜보는 중이면 `dock` 없음 | ✅ `TestNoDockWhenRobotCannotGo` (표 9줄), `TestNoDockWhileWatched` | |
| 위험 수준(10%)에서 작업을 돌려주고 도크로 | ✅ `TestCriticalBatteryHandsTaskBackAndDocks`, `TestCriticalHandBackWatchedAndAsked`, `TestCriticalHandBackDoneFirst` | 로봇이 멈출 때까지 작업은 그 로봇에, `cancelled`가 오면 다른 로봇. 응답 유실 시 같은 `cmd_id`로 물음. 먼저 온 `done`은 마지막 구간이면 DONE, 아니면 다음 구간을 다른 로봇에. 시도 횟수·`failed_on` 그대로, 지켜보기가 끝난 뒤에만 `dock` |
| 위험 수준 경계·조건 | ✅ `TestCriticalThreshold`, `TestCriticalWaitsForOnline` | 10%는 그대로, 9.99%는 돌려줌. STALE 동안은 돌려주지 않고 ONLINE이 되면 돌려줌 |
| 모든 로봇이 충전 중일 때 작업은 대기 | ✅ `TestAllRobotsChargingKeepsTasksQueued` | 5분 동안 `QUEUED`, attempts·dispatches 0, 충전 끝나면 배정 |
| 재시작 뒤 표시 다시 계산 | ✅ `TestBatteryFlagsAfterRestart` | 25% 미만, 또는 90% 미만이면서 DOCKING/CHARGING이면 표시. IDLE 60%는 표시 안 함(알려진 한계, README) |
| `dock` 응답은 작업 응답이 아님, `failed`여도 시간 제한까지 기다림 | ✅ `TestDockAckIgnoredByTasks` | |
| 통합: 배터리 낮은 3대(30·35·40%, 1.5%/m, 10%/s 충전), 작업 20개 | ✅ `TestLowBatteryFleetChargesAndFinishes` (`internal/sim`) | 모두 DONE, 로봇마다 충전 1번 이상·작업 1개 이상, 표시된 동안(또는 25% 미만) 배정 0번, 겹침 검사, attempts 0. 시드 40개로 따로 돌려 모두 통과 |
| 통합: 위험 수준까지 떨어지는 긴 작업 | ✅ `TestCriticalBatteryHandsTasksBack` (`internal/sim`) | 26~28%, 1%/m, 4구간 지그재그 작업 9개, 명령·응답 지연 50~350 ms. 돌려주기 6번(0번이면 실패), 모두 DONE, 겹침 0, attempts 0, 방전 0. 시드 40개 통과 |
| sim 도킹 | ✅ 위 통합 테스트 | `dock`은 어댑터처럼 `accepted`, 추적 중인 goto는 `cancelled`. 자기 도크로 가서 `CHARGING`, 100%에서 `IDLE`. 배터리 0%면 `ERROR (battery_empty)` |
| 테스트가 실제로 잡는지 | ✅ 수동 확인 | 변이 실험: 배정의 충전 필요 검사 제거, 위험 수준의 ONLINE 조건 제거, `dock`의 `task_id` 조건 제거, `<` → `<=`, 구간 보고 조건 제거, 지켜보기 중 도크 금지 제거 — 각각 적어도 한 테스트가 실패 |
| `GET /events?since=` | ✅ `TestEventsSince` | e2e는 이것으로 이벤트를 빠짐없이 모은다 |
| 실제 환경: 배터리 낮은 로봇이 충전하러 가고 그동안 다른 로봇이 일을 받음 | ✅ `scripts/e2e.py` 11번 (시나리오 7개 추가, 모두 31개) | 따로 띄운 `bat_1`(도크 (25,0), 30%, 3%/m, 10%/s): 4 m 작업 → 약 18% → `dock` → 충전. 그 옆 작업 2개는 다른 로봇이 받아 끝냄, 이벤트는 처음 나온 순서가 `low → dock_sent → docking → charged`(재전송 허용), 충전 뒤 다시 배정, 표시된 동안 `bat_1` 배정 0번(`since`로 모은 이벤트, 빠짐 확인). 두 번 연속 통과(약 1분 40초) |

### 4단계 리뷰 (2026-11-03, 모두 수정함)

리뷰어가 1~5를 실패하는 테스트(`TestR4*`)로 증명했다. 그 테스트는 `internal/task/battery_test.go`로 옮겨 고쳤다(아래 테스트 이름).
수정 전 코드(리뷰어 사본 + 새 레지스트리 필드만)에서 새 테스트 11개가 모두 실패하고, 수정 후 통과함을 확인했다.

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 높음 | 위험 수준 돌려받기가 이미 끝난 구간(`done`이 오는 중, 로봇은 IDLE·`task_id` 없음)을 다시 대기열에 넣어 다른 로봇이 또 몬다 | 로봇이 그 구간의 `cmd_id`를 보고할 때만 돌려받는다. 게다가 작업은 로봇이 멈출 때까지 그 로봇에 남기고, 먼저 온 `done`은 그대로 처리 | `TestCriticalSkipsLegFinishedWithDoneInFlight`, `TestCriticalSkipsBetweenLegs`, `TestCriticalHandBackDoneFirst` |
| 2 높음 | 재시작 직후 복구 확인(probe)과 같은 틱에 돌려받아, 서버가 꺼진 동안 끝난 작업을 다시 함 | 1과 같은 조건: 구간을 보고하지 않는 복구 중 작업은 probe에 맡김(보고하면 그 틱에 follow가 이미 확인함) | `TestCriticalSkipsRestoredTask` |
| 3 중 | 표시가 풀릴 때 날아가던 `dock`이 새 goto 뒤에 도착해 goto를 `cancelled`로 끝냄 → 시도 1번, `failed_on` | `dock`을 보낸 뒤 `CmdTTL`+5초 동안은 표시와 상관없이 배정 안 함 | `TestNoTaskWhileDockMayLand` |
| 4 중 (3단계) | 위치 전에 상태가 온 로봇을 0층 (0,0)으로 봄: 건너뛰거나, 0층 지점에 잘못 고르거나, 구간 시간 제한을 잘못 계산 | `fleet.Robot.HasPose`(첫 `pose`에서 설정). 없으면 배정 안 함("no pose yet"), 구간 시간 제한은 위치가 온 뒤 계산. e2e의 `floor == 1` 기다리기 제거 | `TestStateBeforePose` |
| 5 중 | 상태에 `battery`가 없으면(`null` → 0) 영원히 충전 필요, 10분에 `dock` 21번 | `StateMsg.Battery`를 `*float64`로, `Robot.BatteryKnown`. 모르면 표시·도킹 안 하고 배정 대상(로그 한 번). README 문구 고침 | `TestRobotWithoutBattery` |
| 6 낮음 | 거둔 goto가 아직 도착할 수 있는데 `dock`을 보냄 | 그 로봇을 지켜보는 동안은 `dock` 안 보냄 | `TestNoDockWhileWatched`, `TestCriticalBatteryHandsTaskBackAndDocks` |
| 7 낮음 | 90% 전에 멈추는 충전기 → `CHARGING → IDLE` 후 다시 도크, 끝없이 반복 | 한 번은 다시 보내고, 두 번째면 25% 이상은 표시 해제(`charged`, 사유 포함), 25% 미만은 `dock_failed` 후 1분 쉼 | `TestChargerStoppingEarly` |
| 테스트 | 변이가 살아남음: 위험 수준의 ONLINE 조건 제거, `dock`의 `task_id` 조건 제거, 10%에서 `<`→`<=` | 각각 잡는 테스트 추가 | `TestCriticalWaitsForOnline`, `TestNoDockWhenRobotCannotGo`("IDLE but reports a task_id"), `TestCriticalThreshold` |
| 테스트 | sim 통합 테스트가 위험 수준에 닿지 않음 | 지연 + 긴 작업으로 돌려주기가 일어나는 sim 실행 추가 | `TestCriticalBatteryHandsTasksBack` |
| e2e | 불안정 요인: 이벤트 목록을 정확히 비교, 32%−8% ≈ 24%로 여유 1%, 다른 로봇 위치에 따라 `bat_1`이 가장 가깝지 않을 수 있음, 최근 200개 안에 이벤트가 다 있어야 함 | 처음 나온 순서만 비교(재전송 허용), 30%·3%/m(≈18%), `bat_1` 도크를 (25,0)으로(앞 시나리오는 x ≤ 8), `GET /events?since=`로 이벤트를 빠짐없이 모으고 빠지면 실패 | `scripts/e2e.py`, `TestEventsSince` |

## 5단계: 대시보드

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 묶음에 바뀐 로봇만 포함 | ✅ `TestDeltaHasOnlyChangedRobots` | 표시 필드(연결, 작업, 배터리, 층, 방향, 오류)를 하나씩 바꿔 그 로봇만 나오는지, 1 mm 미만 변화와 변화 없음은 묶음 없음, 사라진 로봇은 `removed` |
| 새 구독자의 첫 묶음은 전체 스냅샷, 이어지는 delta와 seq가 맞음 | ✅ `TestFirstFrameIsSnapshot`, `TestLiveFirstMessageIsSnapshot`(httptest + coder/websocket) | 스냅샷 + delta를 적용한 결과가 실제 상태와 같은지 비교 |
| seq는 1씩, 시각은 서버 시계 | ✅ `TestFrameSeqAndTime` | 가짜 시계, 수동 Tick |
| 묶음은 한 번만 만들어 모든 브라우저에 같은 바이트 전송 | ✅ `TestEncodeOncePerFrame` | 구독자 20명: Tick마다 인코딩 1번(`Stats.Encodes`), 받은 슬라이스가 같은 메모리(`&data[0]`). 스냅샷도 seq마다 한 번만 만들어 공유 |
| 느린 브라우저는 오래된 묶음을 버리고 스냅샷으로 다시 맞춤, 빠른 브라우저는 모두 받음 | ✅ `TestSlowSubscriberResyncs`, `TestPartialReaderNeverSeesGap` | 대기열 4, Tick 10번: 느린 쪽은 버림 8·스냅샷 2, 이후 읽는 순서 snapshot·delta·delta, 최종 상태 일치. 빠른 쪽은 delta 10개를 빈틈없이. 띄엄띄엄 읽는 쪽도 200 Tick 동안 빈틈 없음 |
| 동시에 Tick·구독·해지 | ✅ `TestConcurrentSubscribers` | 아래 리뷰 1을 잡은 테스트. 교착 없음, 읽는 쪽 8개가 빈틈 있는 delta를 한 번도 받지 않음, 실제로 버림이 일어났는지도 확인 |
| 구독 해지 뒤 전달 안 됨 | ✅ `TestUnsubscribeStopsDelivery` | 두 번 해지해도 됨 |
| 충전 필요 표시가 나타나고 사라짐 | ✅ `TestNeedsChargeAppearsAndClears`(live), `TestNeedsChargeExposesFlag`(task, 24→60→89.9→90% 히스테리시스) | |
| `GET /`와 `.js`·`.css` 콘텐츠 타입, 없는 파일 404 | ✅ `TestDashboardServed` | Windows 레지스트리가 `.js`를 `text/plain`으로 바꿀 수 있어 확장자별로 직접 지정 |
| 테스트가 실제로 잡는지 | ✅ 수동 확인 | 변이: ① 빈틈 delta 걸러내기 제거 → `TestConcurrentSubscribers` 실패 ② 버린 뒤 스냅샷 대신 delta → `TestSlowSubscriberResyncs`·`TestPartialReader…` 실패 ③ 새 묶음을 버림(옛것 유지) → 세 테스트 실패 |
| 실제 환경 | ✅ `scripts/e2e.py` 12번 (시나리오 4개 추가, 모두 35개) | `GET /` 200 HTML, `/ws/live` 첫 묶음이 모든 로봇의 스냅샷, 작업을 받은 로봇의 x/y가 묶음마다 바뀜(5초에 24번), 간격 중앙값 150 ms, seq 빈틈 없음. pixi 환경에 WebSocket 라이브러리가 없어 스크립트 안에 최소 RFC 6455 클라이언트를 넣음. 두 번 연속 35/35 |
| 대시보드 JS (DOM 없는 부분) | ✅ `node scripts/web_test.js` (23개) | `poseAt` 보간·±π 회전·같은 시각 두 점·외삽(250 ms 상한, 멈춤·IDLE은 안 함), `gridLines` 200줄 상한·1e17 근처 부동소수·NaN, `inRange`, `EventFeed` 빈틈 채우기·채우는 중 도착·중복·서버가 지운 것 건너뛰기·첫 로드·실패 후 재시도·재시작(번호 감소, 실행 ID 변경) |
| 화면 그리기·클릭 | ⬜ 자동 테스트 없음 | `demo.bat` + 앱 안 브라우저로 확인(아래 5단계 리뷰). 데모 GIF·스크린샷은 아직 없음(README "화면"에 찍을 장면만 적음) |

### 5단계 구현 중 찾은 문제 (수정함)

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 높음 | 처음 설계("가득 차면 대기열을 비우고 스냅샷")에서, 전송 쪽이 가장 오래된 묶음을 꺼내 버리는 순간 읽는 쪽이 그다음 묶음을 꺼내 가면 빈틈 위에 delta를 적용한다 | 대기열 항목에 seq와 종류를 붙이고, 읽는 쪽(`Sub.Next`)이 앞 묶음에 이어지지 않는 delta를 버린다. 비운 뒤에는 바로 스냅샷이 들어가 있다 | `TestConcurrentSubscribers` (수정 전 8개 읽는 쪽 모두 "gap") |

### 5단계 리뷰 (2026-11-03, 모두 수정함)

리뷰어 증명: `TestReviewHugePoseNoPanic`(그대로 옮김, 수정 전 패닉), Node 하네스(이벤트 5개 중 3개만 보임, 먼 지점 하나로 `render` 무한 반복).

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 높음 | 위치 x=1e306 → `round()`가 Inf → `json.Marshal` 실패 → `Run` 고루틴에서 패닉 → fleetd 종료 | ① `Ingest`가 `PoseMsg.Check`/`StateMsg.Check`로 거름: 위치 NaN·Inf·\|x\|,\|y\|>1e6 m, yaw NaN·Inf, \|v\|>100 m/s, 배터리 0~100 밖, `silence_s` 0~3600 밖 → 메시지 통째로 버림, 로그 + `Registry.Rejected()`, 마지막 정상 값 유지 ② `round`가 넘치면 원래 값 ③ NaN·Inf가 남은 로봇은 그 로봇만 이전 값으로 두고 건너뜀(`unencodable`) ④ 인코딩 실패는 기록(1·2·4…번째만)하고 묶음을 건너뜀, 상태는 그대로라 다음 묶음에 나감 | `TestReviewHugePoseNoPanic`, `TestIngestRejectsOutOfRange`(경계값 통과 포함), `TestNonFiniteRobotSkipped` |
| 2 높음 | 먼 점 하나(1e17)로 격자 반복이 끝나지 않음(부동소수에서 `x += step`이 그대로), 1e5 m면 프레임마다 3만 줄 | `gridLines`: 축마다 200줄 이하가 되도록 간격을 1·5·10·50…으로 늘리고, 정수 번호 k로 세어 반복 횟수를 제한. \|x\|,\|y\|>1e6 점은 화면 범위에서 뺌. `task.Submit`이 범위 밖 지점을 `ErrInvalid`(API `400`) | `gridLines` 4개(node), `TestSubmitValidation`(범위), `TestPostTask`(1e17 → 400) |
| 3 중 | 이벤트 빈틈을 채우기 전에 `eventSeq`를 올려 채운 결과를 버림(재연결 때도) | `EventFeed`: 이어지지 않는 이벤트는 붙잡아 두고, `since=마지막으로 보인 번호`로 받아 합쳐 순서대로. 채우는 중 도착한 것은 한 번 더 받음. 서버 창 밖으로 밀려난 것만 건너뛰고 알림 | `EventFeed` 6개(node) |
| 3 추가 | 브라우저 확인 중 발견: fleetd 재시작 직후 `/events?n=1`이 `null`을 돌려줘(빈 보관함의 nil 슬라이스) 재연결 처리가 예외로 끝나고, 새 서버의 1번부터 이벤트를 영영 무시 | `Hub.Recent`가 빈 목록 `[]`. 모든 응답에 서버 실행 ID `X-Fleetd-Boot`, 바뀌면 이벤트를 처음부터(번호만 보고는 새 서버가 몇 초 만에 옛 번호를 넘어서 못 잡음) | `TestEventsLimit`(빈 목록이 `[]`, 되돌리면 실패 확인), `TestBootHeader`, `EventFeed` 재시작 2개(node) |
| 4 중하 | 멈춘 연결이 "연결됨"으로 남음 | 2초 동안 바뀐 것이 없으면 `heartbeat` 묶음(현재 `seq` 그대로, 빈틈 없음; 대기열이 찬 구독자는 건너뜀). `Next`는 마지막으로 돌려준 `seq`와 같은 heartbeat만 넘김. 브라우저는 위치 연결에서 5초 동안 아무것도 없으면 "응답 없음" 후 재연결 | `TestHeartbeat`, `TestStaleHeartbeatDiscarded`, e2e 묶음 순서 검사(heartbeat는 같은 `seq`) |
| 5 낮음 | 끊김(200 ms 지연이 묶음 간격에 가까움) | 지연 350 ms + 최신 위치를 지나면 이동 중일 때만 v·yaw로 최대 250 ms 외삽. 위치가 같아도 속도·모드가 바뀌면 새 표본(멈춤이 외삽을 끝냄) | `poseAt` 5개(node) |
| 6 낮음 | 다른 사이트가 단순 POST로 작업 생성·취소 가능 | 두 POST: `application/json` 필수(`415`), `Sec-Fetch-Site: cross-site/same-site`·다른 `Origin`은 `403`. 클릭 한 번 작업은 유지하고 3초 "취소" 알림(되돌리기) | `TestPostsRefuseOtherSites`(두 경로 × 7가지 + 같은 출처 201/200) |
| 7 낮음 | e2e WebSocket 클라이언트가 프레임 중간 시간 초과에 헤더를 잃음 | 프레임 전체가 버퍼에 올 때까지 소비하지 않고, 조각난 메시지도 호출 사이에 보관 | 가짜 소켓으로 헤더·길이·본문·조각을 시간 초과 사이에 나눠 보내 확인(일회성 스크립트) |
| 변이 | 살아남던 변이: 스냅샷 `Time` 빠짐, `Run`이 `Tick`을 안 부름, `/ws/live` 쓰기 시간 제한 없음, 구독 해지 안 함, `NeedsCharge`가 판단 전 로봇 무시 | 각각 잡는 테스트 추가. 구독자 한도(64, 넘으면 `503`)와 `frames == nil`의 `503` 테스트도 추가 | `TestSnapshotCarriesFrameTime`, `TestRunTicks`, `TestLiveSlowClientClosed`(안 읽는 클라이언트는 닫히고 읽는 쪽은 계속 받음), `TestLiveSubscriberLimit`(닫으면 `Subscribers` 0으로), `TestNeedsChargeBeforeFirstTick`, `TestMaxSubscribers`, `TestLiveOffIs503`. 각 변이를 실제로 넣어 실패 확인 |

브라우저 확인(`demo.bat`, 앱 안 브라우저): 클릭 → 3초 알림 → "취소" → `CANCELLED`; 2층 작업을 만들면 층 버튼에 2층이 생기고 누르면 그 층 지점만 보임; 작업 목록 "취소"로 수행 중 작업 취소(로봇 멈춤);
`drop_comms`를 8초마다 보내 robot_2를 STALE(흐린 점선 원) → OFFLINE(어두운 원에 X) → 복구; fleetd를 끄면 "연결 끊김" 띠와 두 배지 "끊김", 다시 켜면 띠가 사라지고 "서버가 다시 시작됨" 알림 뒤 새 서버의 이벤트가 처음부터 보임.
확인하지 못한 것: 연결은 살아 있는데 서버가 멈춘 경우의 "응답 없음"(서버를 죽이면 소켓이 바로 닫혀서 재현 못 함, heartbeat 쪽은 Go 테스트로만), IN_ELEVATOR 그리기(6단계 전이라 mock이 보내지 않음. 6b에서 `demo.bat`로 확인함).

## 6단계: 목적층이 다른 로봇 함께 태우기 ⭐ (6a: 서버·sim·프로토콜, 6b: 실제 프로세스)

6a는 서버 쪽만 한다: 계획 함수, 운행 지휘, 작업 단계 연결, `internal/sim`의 엘리베이터와 탑승 로봇, 규약 문서 [ELEVATOR.md](ELEVATOR.md).
ROS mock 로봇·어댑터·mock 엘리베이터·대시보드·e2e는 6b(이 문서대로 구현). 아래 테스트는 모두 가짜 시계라 1초 안에 끝난다.

### 계획 함수 (`internal/elevator/plan_test.go`)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 1층에서 3·7·5층 → 탑승 7·5·3, 3층 로봇이 칸 0(문 쪽), 입력 순서와 상관없음 | ✅ `TestPlanTable` (15줄) | 같은 층 2대(이름순으로 문 쪽), 칸 부족(가까운 층부터, 나머지 `capacity`), 방향 다름(`direction`), 방향을 안 주면 다수·같으면 가까운 쪽, 내려갈 때 반대, 지하층, 0대·1대, 목적층 = 현재 층(`same_floor`, 태우지 않음), 같은 로봇 두 번(`duplicate`), 칸 0 |
| 칸 수만큼 나눠 여러 번 | ✅ `TestPlanTripsSplitsByCapacity` | 7대·2칸 → 운행 3번(가까운 층부터), 반대 방향은 마지막 운행의 `deferred`에 한 번만 |
| 🔧 무작위 입력 규칙 검사 | ✅ `TestPlanProperties` (5,000개) | 매 정차 층에서 내릴 로봇이 남는 로봇보다 모두 문 쪽, 늦게 타는 로봇이 더 문 쪽, 칸 수 이하, 방향 하나, 정차층 = 목적층·이동 순서, 모든 로봇이 정확히 한 번 타거나 한 번 미뤄짐, 결정적, 입력 순서를 섞어도 같은 결과, `PlanTrips`로 나누면 같은 방향 로봇이 모두 정확히 한 번 탐 |

### 운행 지휘 (`internal/elevator/car_test.go`, 가짜 시계·가짜 링크)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 3초 모으기 | ✅ `TestCarBatchWindow` | 2초 뒤 온 로봇은 함께, 계획 뒤 온 로봇은 다음. 2.75초엔 계획 없음, `call` 1번 |
| 모두 수락 + 문 열림 전에는 turn 없음 | ✅ `TestCarAllAcceptBeforeBoarding` | 2/3 수락이면 turn 0, 마지막 수락 즉시 7층 로봇부터. `opening`이면 turn 없음 |
| 수락 없음 → 중단 | ✅ `TestCarAbortOnMissingAccept` | 10초에 중단, 수락 안 한 로봇만 fault, `release`, turn 0, 바로 재계획 가능 |
| 엘리베이터 응답 없음 → 중단 + 30초 쉼 | ✅ `TestCarAbortWhenElevatorSilent`, `TestCarAbortWhenCarNeverOpens` | 상태 5초 없음 / 다른 층에 머묾. 로봇 fault 없음, `call`은 같은 `cmd_id`로 재전송 |
| 한 대씩: 다음 turn은 앞 turn이 닫히고 `door_zone: 0`이 온 뒤에만 | ✅ `TestCarOneAtATime` (리뷰 뒤 고침) | 다른 `n`·로봇·운행의 `zone` 무시, 문 구역에 로봇이 있는 동안 14초·40초가 지나도 다음 turn 없음(같은 `n` 재전송만), 비면 같은 로봇 재시도, 또 시간 초과면 `unsure_in` 후 다음, 하차는 칸 순서 |
| 문 유지·해제 | ✅ `TestCarDoorHoldAndRelease` | 5초마다 `hold`, 마지막 로봇 전 `release` 0번, 끝에 정확히 1번 |
| 하차 실패 → 재시도 → 다음 층(뒤 로봇도) | ✅ `TestCarExitRetryThenCarry` | 넘겨진 로봇들이 다음 층에서 먼저(문 쪽) |
| 마지막 층 하차 실패 → 사람 필요 | ✅ `TestCarStuckAtLastStop` | `stuck` notice, 5분 동안 새 운행 없음, 임대 경보, ONLINE으로 보이면 반환 |
| 임대 만료는 경보만 | ✅ `TestCarLeaseFreesOnlyWhenSafe` | 로봇이 안에 있는 동안 반환·`release` 없음 |
| 재시작: retained 운행 이어받기 | ✅ `TestCarRestoreMidTrip` | 같은 `n`으로 turn 재전송, `hold` 다시, 새 실행 ID의 `cmd_id`. `planned`면 fault 없이 중단 |
| 늦은 `zone`, 빼기 규칙 | ✅ `TestCarLateAnswersAndDrop` | 대기 중 로봇만 뺄 수 있음, 늦은 `clear`로 확정, 탑승 시간 초과 뒤 로비에서 ONLINE이면 `left` |
| 설정 파일 | ✅ `TestParseConfig` | 기본값, 층별 로비, timing 일부, 잘못된 파일 7가지, `""`·`none` |

### 작업 연결 (`internal/task/ride_test.go`)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 로비 `goto`(`.L`) → 대기 → `ride`(`.R`) → 지점 `goto` | ✅ `TestRideLegPhases` | 탑승 뒤 60초 무응답·OFFLINE이어도 작업 유지, 이벤트 순서 전체, 자기 retained 메아리 무시 |
| 엘리베이터가 서지 않는 층 → FAILED, 같은 층은 그냥 goto | ✅ `TestRideNoElevatorForFloorFails` | |
| 두 번째 구간이 다른 층 | ✅ `TestRideSecondLegOnOtherFloor` | |
| `can_ride: false` 로봇은 다른 층 작업을 못 받음 (없으면 true, 더 가까워도 건너뜀, 자유 로봇이 그뿐이면 QUEUED로 두고 뒤의 같은 층 작업은 배정, 엘리베이터 없으면 영향 없음, 배정 뒤 false로 바뀌면 시도 1번 쓰고 재배정) | ✅ `TestCanRideDefaultsToTrue`, `TestRideSkipsRobotThatCannotRide`, `TestRideCrossFloorTaskWaitsForRider`, `TestCannotRideIgnoredWithoutElevators`, `TestRideFlagFlipAfterAssignmentRequeues` | `GET /robots`에 `can_ride`(`TestRobots`) |
| 중단 → 같은 로비에서 다시 대기 | ✅ `TestRideAbortReturnsRobotsToTheQueue` | 옛 ride를 겨냥한 `cancel`, 시도 0, 쉰 뒤 새 `cmd_id` |
| 수락 안 한 로봇만 책임 | ✅ `TestRideMissingAcceptBlamesThatRobotOnly` | 3초마다 재전송 |
| 배터리 위험 | ✅ `TestRideCriticalBattery` (3가지) | 로비 대기·`planned`는 돌려줌(시도 0), 탑승 turn 뒤엔 아무 명령 없음(`dock`도), 내린 뒤 `goto`에서 돌려줌 |
| 엉뚱한 층 하차 | ✅ `TestRideWrongFloorReplans` | 그 층 로비로, `replanned` |
| 사용자 취소 | ✅ `TestRideUserCancel` | 대기 중이면 빠짐, 탄 뒤면 내려 줌 |
| 재시작 | ✅ `TestRideRestartMidTrip`, `TestRideRestartWithoutTrip` | 운행 이어받으면 유예 뒤에도 재배정 없음 / 운행 없으면 물어보고 `cancel` 후 재대기 |
| 로봇 이벤트 seq 순서 | ✅ `TestRobotEventsRecordedInOrder` | 3·1·2 → 1·2·3, 중복 무시, 빈 번호 1초 뒤 건너뜀, 새 `boot` |
| `GET /elevators` | ✅ `TestElevators` (`internal/api`) | |

### 통합 (`internal/sim/elevator_test.go`)

모든 테스트는 50 ms마다 ① 문 구역에 2대 이상 ② 한 작업을 두 로봇이 몲 ③ OFFLINE(허용한 테스트 제외) ④ 구간 완료 때 로봇의 실제 층 ≠ 지점 층이면 실패.

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 1층에서 3·7·5층 함께 | ✅ `TestRideThreeFloorsTogether` | 운행 1번, 문 기록 정확히 7·5·3 탑승 / 3·5·7 하차, 문 구역 최대 1대, 3대 모두 IN_ELEVATOR·OFFLINE 0, 로봇 이벤트 seq 순 |
| 하차 실패 1번 / 2번(넘김 후 되돌아옴) | ✅ `TestRideExitFailsOnceThenSucceeds`, `TestRideExitFailsTwiceIsCarried` | |
| 마지막 층 문 막힘 | ✅ `TestRideStuckAtLastStopNeedsHuman` | 다른 로봇에게(시도 1), 엘리베이터 `blocked`, `Rescue` 후 완료 |
| 엘리베이터 응답 없음 | ✅ `TestRideElevatorUnresponsiveAborts` | 탑승 turn 0, 살아나면 완료·시도 0 |
| 엉뚱한 층 하차 | ✅ `TestRideWrongFloorExitIsReplanned` | |
| 예고보다 늦은 와이파이 / 훨씬 늦음 | ✅ `TestRideLinkLaterThanAnnounced`, `TestRideLinkLostAfterExit` | STALE만·같은 로봇 완료 / OFFLINE → 다른 로봇(시도 1) |
| 작업 10개·로봇 4대 | ✅ `TestRideMixedLoad` | 모두 DONE·시도 0, 함께 탄 운행 1번 이상 |
| 시드 반복 | ✅ 수동 | 임시 테스트로 시드 60개 × 2종(지연, 응답 15% 유실 + 하차 실패 + 엉뚱한 층): 모두 완료, 문 구역 최대 1대 |
| 실제 환경 | ✅ 6b (아래) | |
| 🔧 엘리베이터 벤더 어댑터 2종 | 🔧 | 6b 이후 |

### 변이 실험 (6a)

파일 백업 → 한 곳 바꿈 → `go test` → 복원, 40여 개. 모두 적어도 한 테스트가 실패했다. 처음 살아남은 3개는 테스트를 고쳐 잡았다:
`planned` 운행 재시작 시 이어가기(다른 경로로 중단돼 통과 → 사유·fault 검사 추가), 자기 실행의 retained 운행 수락(늦은 메아리 검사 추가),
배터리 변이 2개는 컴파일 오류라 다시 씀. sim에서만 살아남는 2개(탑승 중 로봇에 평소 규칙, 중단 시 `cancel` 생략)는 단위 테스트가 잡고, sim에서는 IN_ELEVATOR·지켜보기가 이중 안전으로 막는다.

### 6a 리뷰 (2026-11-03, 모두 수정함)

리뷰어 증명 R1~R6: 원래 테스트를 수정 전 코드(리뷰어 사본)에서 돌려 6개 모두 실패, 수정 후 코드에 그대로 넣어 R1·R2·R3·R5·R6 통과(R4 원본은 흐름이 바뀌어 "no turn"으로 멈춤: 바뀐 흐름에 맞춘 것을 넣음).
옮긴 테스트는 `internal/elevator/review_test.go`, `internal/task/review_ride_test.go`, `internal/sim/review_test.go`.

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 높음 | 하차 turn에 답이 없으면 `unsure_out` → 다시 turn을 받지 못해 엘리베이터가 영원히 묶이고 작업은 영원히 RUNNING | 열린 turn을 5초마다 같은 `n`으로 재전송. 하차 시간 초과는 "아직 안에 있음"(실패한 시도) → 재시도 → 다음 층으로 → 마지막 층이면 `stuck`(작업은 다른 로봇에게, 한 번만), `blocked`에서는 1분마다 그 층에서 문을 다시 열고 하차 turn을 다시 줌. 임대 만료 시 확인 안 된 로봇마다 `stuck` 알림. `unsure_out` 없앰 | `TestReviewUnansweredExitIsRetriedNotStranded`(R1), `TestReviewTaskSettledAfterUnansweredExit`(R6), `TestReviewLeaseSettlesRobotInside` |
| 2 높음 | 이어받은 운행이 옛 `boot`를 유지 → 자기 retained 메아리를 다시 받아 좀비 운행·반복 | 이어받을 때 `boot`를 새 실행 ID로, 끝난 운행 ID를 기억해 무시, 끝나면 마지막 단계를 보낸 뒤 retained 메시지를 지움(`Link.ClearTrip`, 빈 retained 발행) | `TestReviewRestoredTripEchoIgnored`(R5), `TestReviewTripPublishRetriedAndCleared` |
| 3 높음 | 시간 초과 뒤 앞 로봇의 문 구역이 정리되지 않았는데 다음 로봇에게 turn → 문 구역에 두 대. sim 문 이동(2초)이 시간 초과(15초)보다 짧아 잡지 못함 | 엘리베이터 상태에 `door_zone`(문 구역 수: 엘리베이터 센서, mock은 `zone` `enter`/`clear`/`failed`로 셈). 다음 turn은 앞 turn이 닫힌 **뒤에 받은** `door_zone: 0` 상태가 있어야 함. 시간 초과는 같은 로봇의 재시도일 뿐 뒤 로봇을 움직이지 않음. sim 로봇이 `enter`를 보내고, 문 이동 시간을 시간 초과보다 길게 할 수 있음 | `TestReviewTimeoutDoesNotGrantNext`(R2), `TestCarOneAtATime`(고침), sim `TestRideRobotsSlowerThanTurnTimeout`(문 이동 40초 > turn + 재시도 30초, 문 구역 최대 1대, 시작 뒤 반드시 자기 끝) |
| 4 중상 | 문이 닫힌 채로 turn(재시작 뒤 유지 시간 만료, 하차 중 스스로 닫힘) | turn마다 "최근 5초 안에 그 층·그 운행으로 문 열림"을 확인, 아니면 turn 없이 같은 층 `goto`(5초마다)로 다시 염. 재시작 때는 `hold` 대신 `goto`, 문이 열린 것을 본 뒤 turn 재전송. 탑승·하차 중 무응답이면 `overdue`("car not answering") | `TestReviewNoTurnWithDoorClosed`(R3), `TestCarRestoreMidTrip`(고침) |
| 5 중 | 늦게 온 하차 `failed`가 로봇을 차 뒤쪽 층으로 넘겨 하차 순서가 깨짐 | 넘김은 지금 정차층 기준 다음 층만. 늦은 하차 `failed`는 무시(시간 초과가 이미 "안에 있음"으로 셈). 하차 turn은 안에 있을 수 있는 로봇 중 가장 문 쪽 로봇에게만, 그 로봇이 여기서 안 내리면 뒤 로봇도 넘김 | `TestReviewLateExitFailureKeepsOrder`(R4), `TestReviewCarryOnlyFromTheFailedRobotBack` |
| 6 중 | 넘겨진 로봇이 나올 자리를 모름 | `turn`에 그 층의 로비 `lobby`(x, y). sim은 turn의 로비로 나옴 | `TestReviewNoTurnWithDoorClosed`(lobby), sim 테스트 전부 |
| 7 중 | 발행 대기열이 차거나 브로커가 끊기면 retained 운행이 오래된 채로 남음 | 운행을 10초마다, MQTT 재연결 때(`mqttx.Client.Connected`, `RepublishTrips`) 다시 발행, 발행 실패는 다음 진행에서 재시도. main의 `outbox`(별도 파일): 로봇 명령 512개 제한(예전과 같음), 엘리베이터 명령·turn은 거절 없음(2,048개 넘으면 가장 오래된 것을 버리고 기록: 지휘자가 재전송), 운행은 버리지 않고 같은 엘리베이터의 새 것으로 대체, 실패하면 1초 뒤 재시도 | `TestReviewTripPublishRetriedAndCleared`, `TestOutboxLimitsAndCoalescing`, `TestOutboxRetriesTrips` |
| 8 중 | 탑승 전 취소 대상을 `cmd_id`의 첫 `.x`로 잘라 냄(서버 실행 ID가 `x`로 시작할 수 있음) | `cancel`에 `target` 필드. 문서·sim 갱신 | `TestRideAbortReturnsRobotsToTheQueue`, `TestReviewReleaseElevator` |
| 9 낮중 | 사람이 처리한 뒤 묶인 엘리베이터를 풀 방법이 없음 | `POST /elevators/{id}/release`(같은 JSON·사이트 규칙): 운행 끝, retained 지움, 탑승 전 로봇은 다시 대기, 확인 안 된 로봇의 작업은 시도 없이 대기열로 + 지켜보기, `released` 이벤트 | `TestReleaseElevator`(api: 404·409·415·403·200), `TestReviewReleaseElevator`, `TestReviewForceRelease` |
| 10 낮음 | `zone` 값 검증 없음, 자기 층 대기 로봇이 영원히 대기, 0인 시간 설정, `hold_refresh_s ≥ hold_for_s`, 낡은 주석 | 모르는 action·event·이 turn과 다른 action은 기록 후 무시(시도 안 씀). 목적층 = 대기 층이면 `no_ride` → 그 구간을 그냥 `goto`로. timing은 모두 0보다 커야 하고 `hold_refresh_s < hold_for_s`. proto 주석 고침 | `TestReviewZoneValuesValidated`, `TestReviewSameFloorWaiterNeedsNoRide`, `TestReviewNoRideNeeded`, `TestReviewParseTimingRules` |
| 리뷰 중 발견 | sim의 turn 유실 테스트가 찾음: 탑승 turn이 열려 있는(유실된) 로봇이 로비에서 ONLINE으로 보이자 "타지 않음"으로 처리해 운행을 버림 | 열린 탑승 turn의 로봇은 와이파이로 정리하지 않음(재전송·재시도가 처리). 늦게 온 탑승 `clear`는 재시도 대기 중이어도 받아들임, 이미 한 일의 새 turn에 로봇은 `clear`로 답함 | sim `TestRideLostTurnAndZone`(turn 2번 유실 + 답 1번 유실) |
| 문서 | ELEVATOR.md 모순("시간 초과면 다음 로봇") 등 | "한 대씩"은 `door_zone` 규칙으로 보장. 다른 운행·`done` 뒤 turn 무시, retained `trip.turn`으로 움직이지 않음, 차 안에서 exit turn이 없으면 기다림, `hold`는 닫힌 문을 다시 열지 않음(같은 층 `goto`로 엶, sim도 같게), mock의 문 구역 세기(turn과 무관), `stuck`과 운영자 반환, `cancel.target`, 어댑터는 계획보다 `cmd_id` 중복을 먼저 확인 | |

**누락 테스트·살아남은 변이 8개**(리뷰어): 모두 잡는 테스트를 넣고 변이를 다시 넣어 확인 — 하차 시간 초과를 "내림"으로(`TestReviewUnansweredExit…`, `…LateExitFailure…`),
문 쪽 로봇 무시하고 넘김(`TestCarExitRetryThenCarry`, `…CarryOnlyFromTheFailedRobotBack`), 와이파이 정리에서 "turn 뒤에 들림" 무시(`TestReviewAccountNeedsReportAfterTurn`),
상태 중복 제거 없음(`TestReviewStateDeduplicated`), 임대 만료가 탑승 전 운행을 중단 안 함(`TestReviewLeaseAbortsUnboardedTrip`), 이어받은 ride를 확인 표시 안 함(`TestReviewRestoredRideConfirmed`),
늦은 하차 `failed`로 다시 넘김(`TestReviewLateExitFailureKeepsOrder`), 기다리는 중 같은 seq 이벤트 중복 기록(`TestReviewRobotEventDuplicateWhileWaiting`).

**변이 실험(수정 후):** 위 8개 + 새 규칙 24개(문 구역 확인 없이 turn, 문 닫힌 채 turn, 시간 초과에 다음 로봇, turn 재전송 없음, 이어받을 때 옛 boot, 끝난 운행 기억 안 함, retained 안 지움,
주기 재발행 없음, 발행 실패 재시도 없음, `blocked` 재시도 없음, 임대 만료 `stuck` 알림 없음, zone 검증 없음, `no_ride` 무시(car·task), 탑승 turn 열린 로봇 정리, 운영자 반환 후 재배정 없음,
`cancel.target` 없음, outbox 운행 재시도·대체 없음, 설정 0·hold 순서) + 1차 변이 23개 다시: 모두 적어도 한 테스트가 실패.
sim 쪽에서 처음 살아남은 2개(문 구역 확인 없이 turn, turn 재전송 없음)는 sim 테스트를 강하게 해서(문 이동 40초, turn 2번 유실) 잡음.
살아남은 1개는 동치: `RestoreTrip`의 "자기 boot면 무시"는 이제 "끝난 운행 무시 + 운행 중이면 무시"에 포함된다(이 실행의 운행은 진행 중이거나 끝났거나 둘 중 하나).

### 6b: 실제 프로세스 (mock 엘리베이터·ROS mock 로봇·어댑터·대시보드·e2e)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| mock 엘리베이터 동작 (`cmd/mockelevator/car_test.go`) | ✅ 5개(+ 리뷰 4개 = 9개) | `call` 열고 `hold_s` 뒤 닫힘(예약은 남음), `goto`는 문 구역에 로봇이 있으면 떠나지 않음·`door_zone` 즉시 발행·지나는 층 상태, `door_zone`은 로봇 이름 집합(중복 `enter`·들어온 적 없는 `clear`·두 번 온 `clear`), 문 막힘 +1, `hold`는 닫힌 문을 다시 열지 않음·같은 `cmd_id` 한 번·같은 층 `goto`로 다시 엶·다른 운행 `hold` 무시·`release`, 응답 없음은 명령 무시·발행 0, 1초마다 상태 |
| 어댑터 탑승 절차 (`adapter/test_ride_logic.py`, `RobotRide`) | ✅ 29개(+ 리뷰 13개 = 42개) | 수락 조건 7가지(계획 없음·잘못된 계획·ERROR·다른 층·로비에서 멂·차 안·상태 모름), 대기 중 새 ride가 옛것을 `cancelled`, 탑승 시작 뒤 새 ride 거절, `cancel.target`: 지금 ride만·옛 ride 취소는 `failed`·탄 뒤 `failed`·ride와 무관하면 goto 규칙, `x`로 시작하는 실행 ID. 탑승 turn: 이벤트·`BOARDING`+`silence_s`·`enter`·이동 순서, 끊김 시작, 이벤트는 쌓임. 탑승 실패면 로비에서 와이파이 복귀. 다른 로봇·다른 운행·잘못된 turn 무시, 움직이는 중 같은 `n` 무시, 답한 `n`은 같은 답(같은 seq), 이미 한 일의 새 `n`은 `clear`, turn 유실 주입. 차 층 따라가기, 하차는 turn의 로비로, 1초 뒤 `pose`→`state`→이벤트(seq 순)→`done`(+층), 하차 실패면 칸에서 기다림(스스로 안 내림)·재시도·넘겨진 층에서 내림·`done` 층 5, `done` 뒤 turn 무시, 낡은 이동 보고 무시, seq는 운행을 넘어 계속 |
| 대시보드 엘리베이터 (`scripts/web_test.js`) | ✅ 8개 추가(31개) | 칸별 로봇(나간·대기 로봇 제외, 넘겨짐 표시, 잘못된 칸 번호 무시), 차 안 로봇 → 차의 층, 칸 상자와 칸 위치(어댑터와 같은 값), 이벤트 detail 나누기, 경보(응답 없음·막힘·갇힘·임대·차례 시간 초과·이 운행의 overdue만·문 구역 2대·쉬는 중) |
| e2e 함께 타기 (`scripts/e2e.py` `mixed_ride`) | ✅ 12단계 | 1층 `lift_1..3` → 3·7·5층 동시: 운행 **1번**, 계획 탑승 7·5·3·정차 [3 5 7], 실제 `zone clear` 순서 탑승 7·5·3 / 하차 3·5·7(각자 자기 층), 엘리베이터 상태를 계속 구독해 **`door_zone` 최대 1**(1인 상태를 실제로 봤는지·로봇들의 enter/clear로 따로 센 최대도 1), 3대 모두 `silence_s`를 보내고 IN_ELEVATOR, OFFLINE 0, **차 안에 있는 동안 그 로봇의 `fleet/*` 메시지 0개**, 로봇 이벤트 4개(`ride_accepted`·`boarding`·`boarded`·`exited`)가 seq 순이고 `boarded`·`exited`는 하차 뒤에 도착(쌓였다가 보냄)·fleetd가 `robot_event`로 기록, `done`의 `floor` = 목적층, 작업 DONE·로봇 층 3·5·7·재배정 없음, `released` 1번·운행 지워짐 |
| e2e 하차 실패 (`exit_failure`) | ✅ 6단계 | `fx_1`, `fx_2` → 4·6층, 4층 하차 2번 실패(`--exit-fail-count 2`): 4층 `failed` 2번 → 넘겨져 6층에서 **넘겨진 로봇이 먼저**(문 쪽) → 다른 로봇, fleetd `exit_failed` 2번 이상·`replanned`("exits at floor 6"), `done` 층 6 → 다시 내려가는 운행 → `done` 층 4, 작업 둘 다 DONE·층 4·6·재배정 없음, `door_zone` 최대 1. 앞 시나리오의 로봇은 끝낸 뒤 프로세스를 끈다(위층의 놀고 있는 로봇이 층당 1000 m 가산으로도 더 가까워 작업을 가져가기 때문) |
| 데모 | ✅ 수동 | `scripts\demo.bat` + `demo_tasks.ps1 up/down`: 브라우저에서 칸별 로봇·문 구역·단계·차 안 로봇 그림 확인 |

**6b 구현 중 찾은 문제(고침):** ① 어댑터가 탑승 turn에서 끊김을 먼저 켠 뒤 `boarding` 이벤트를 보내려 해 이벤트가 사라짐(seq 2 빠짐) — e2e의 이벤트 이름 검사가 잡음. 순수 로직이 내준 이벤트는 바로 보내고, 끊긴 동안의 것은 로직이 쌓는다.
② 처음 e2e에서 하차 실패 시나리오의 작업을 위층에 있던 앞 시나리오 로봇이 가져감(배정은 층 차이 × 1000 m) — 시나리오 로봇을 끝나면 내리게 고침. 서버 동작은 규칙대로다.

### 6b 리뷰 (2026-11-03, 모두 수정함)

리뷰어 증명: `test_review6b.py` 3개(잘못된 동작을 단언)와 `review6b_test.go` 2개. 뒤집어서 회귀 테스트로 넣었다.

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 중 | 차 안 로봇에게 exit turn이 끝내 안 오면(운영자 반환, retained 운행 유실) 영원히 끊김 | 차 안(`RIDING`, 움직임 없음)에서 그 운행이 지워지거나 `done`/`aborted`가 되거나 다른 운행으로 바뀌거나 60초 동안 소식이 없으면 끊김을 끝냄: `pose`(차의 층)·`state`·쌓인 이벤트 + `exit_failed` → ride `failed`(+`floor`), 꺼낼 때까지 `ERROR`("needs a human"). 꺼내면(`in_car: false`) `rescued`. 운행 중에 꺼내면 `rescued` 뒤 하차처럼 `done`. mock 로봇에 `rescue` 서비스(+ 어댑터 명령 `rescue`). 운영자 절차는 ELEVATOR.md 6장 | `test_review_trip_released_with_the_robot_inside`(원래 `test_no_way_out…`), `test_trip_cleared_or_replaced…`(3가지), `…before_boarding…`, `…during_a_move…`, `test_trip_not_heard_for_a_long_time_is_gone`, `test_rescued_while_the_trip_runs_comes_out_done`, 배선 `test_cleared_trip_ends_the_blackout_and_rescue_command`, e2e 갇힘 → 반환 → 꺼냄 |
| 2 중 | `elevator_move` 유실·로봇 재시작이면 어댑터가 영원히 `BOARDING`+끊김 | 움직임 기한 `turn_timeout_s` + 30초(`--move-timeout-extra`). 넘으면 `failed`("robot did not finish the move"), 탑승이면 끊김을 끝내고 `WAITING_ELEVATOR`. 로봇이 그 움직임을 `running`으로 알리는 동안은 기다림(문 구역 안) | `test_review_board_move_never_reported_fails`(원래 `…waits_forever`), `test_slow_move_still_running_is_waited_for`, `test_exit_move_timeout…`, e2e 느린 문 |
| 3 낮 | 새 `n`으로 움직이는 중에 옛 `n`의 turn이 늦게 오면 옛 답을 다시 보냄 → mock 엘리베이터가 문 구역에서 뺌 → 문이 로봇 위로 닫힐 수 있음 | 움직이는 중에는 모든 turn 무시. mock 엘리베이터는 같은 운행에서 그 로봇의 마지막 `enter`보다 작은 `n`의 답을 무시(운행이 바뀌면 다시 셈: e2e가 처음 고친 판을 잡음) | `test_review_old_n_is_not_answered…`(원래 `test_old_n_reanswer…`), `TestReviewStaleAnswerKeepsRobotInZone`, `TestZoneNumbersRestartPerTrip` |
| 4 낮 | mock 로봇이 움직이다 고장 나면 문 구역 안에서 `failed`를 보고 → `door_zone` 0인데 문을 막고 있음 | 고장이면 그 자리에 멈추고 움직임은 `running`으로 남음(어댑터도 기다림). `reset_error`가 시작점으로 되돌린 뒤 `failed` | 어댑터 쪽 `test_slow_move_still_running_is_waited_for`(running이면 답 없음). mock 로봇 자체는 ROS라 자동 테스트 없음(수동 확인 안 함) |
| 5 낮 | 닫힌 문 앞에서 로봇이 들어와도 다시 열지 않고, `door_zone` 1로 출발할 수 있음 | 서 있는 차는 `door_zone` > 0이면 닫힌·닫히는 문을 다시 열고, `door_zone` > 0이면 출발하지 않음 | `TestReviewEnterOnClosedDoorReopens`(goto 중에도 이동 없음), `TestReviewBlockedDoorNeverTravels` |
| 6 낮 | 로비에 있는데 `exit` turn이 오면 아무 답 없음 | `failed`("not in the car"), 문서화 | `test_review_exit_turn_at_the_lobby_is_answered_failed` |
| 테스트 | 어댑터 배선(`fleet_adapter.py`) 테스트 없음 | 가짜 rclpy·메시지·paho 모듈로 `FleetAdapter`를 띄움 | `test_adapter_wiring.py` 8개: 수락·구독, 탑승 turn의 순서(이벤트 → BOARDING 상태 → enter), 끊긴 동안 `fleet/*` 0(상태·위치·이벤트·응답), 끊긴 동안 명령 버림(명령 장부에도 없음), `run()` 순서, 내린 뒤 `pose`→`state`→`done`과 계획 없는 같은 `cmd_id`에 `done` 다시, 탑승 중 MQTT 재연결(구독 복구, 로봇 메시지 없음), 운행 지워짐 → ERROR + `rescue` 명령 |
| e2e | 끊김 검사가 하차 `clear`까지만, 문 규칙을 엘리베이터 상태로 확인하지 않음, 운행이 정확히 1번이어야 해서 흔들릴 수 있음, 답을 원본 메시지 목록으로 셈 | 끊김은 `clear` + 1초까지, 첫 메시지가 0.9초 이상 뒤. 모든 `enter` 앞(앞 답 이후, ±0.25초: 다른 클라이언트라 도착 순서가 바뀔 수 있음)에 문 열림·`door_zone` 0·정지 상태, 이동 중 `door_zone` > 0 상태 없음, turn마다 `enter` 1번. 로봇을 로비 3~4.2 m에 두어 1초 안에 도착(모음 3초), "세 대를 모두 태운 운행"으로 검사. 답은 (운행, `n`)마다 첫 답. 시나리오마다 끝난 로봇을 내림. 시작할 때 브로커에 남은 retained 운행·상태를 지움(죽은 앞 실행의 `blocked` 운행을 새 fleetd가 이어받아 모든 시나리오가 멈춘 것을 겪음) | 엘리베이터 e2e 36단계 |

**6b에서 하지 않은 것:** 엘리베이터 응답 없음·문 막힘은 주입 옵션만 있고 e2e 단계는 없다(`internal/sim`·`internal/elevator`의 Go 테스트와 `cmd/mockelevator` 테스트가 검사). 어댑터를 탑승 중 재시작하면 ride를 잃는다(로봇이 `in_car`이면 ERROR로 보고해 배정은 막지만 운행을 이어받지는 않음: 사람이 꺼내야 함). 서버 재시작 중 탑승 e2e 없음. mock 로봇의 "움직이다 고장" 처리(리뷰 4)는 자동 테스트가 없다.

## 7단계: 마무리

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| CI에서 `go test -race` | ✅ 작성 (CI 실행은 push 뒤) | `.github/workflows/ci.yml`의 `go` 잡: gofmt 검사·`go vet`·`go test -race -count=1 ./...`·Windows 교차 빌드. 이 PC는 C 컴파일러가 없어 `-race`를 못 돌리므로 경합은 코드를 읽어 찾았다: `api.writeTimeout`을 테스트가 바꾸고 되돌리는 동안 앞 테스트의 WebSocket 핸들러 고루틴이 읽을 수 있었다(httptest는 하이재킹된 연결을 기다리지 않음) → `api.New`에서 한 번 읽어 핸들러에 넘김. 나머지 동시성 테스트(`live` 동시 구독, 레지스트리 이벤트 순서, outbox, 허브)는 잠금·원자값·채널로만 공유함을 확인. 전체가 `-race` 없이 몇 초라 10배 느려져도 짧다 |
| CI: 대시보드 JS, 어댑터 pytest | ✅ 작성 | `node` 잡(`node scripts/web_test.js`), `python` 잡(Python 3.12 + pytest + paho-mqtt, ROS 없음). `test_adapter_wiring.py`는 rclpy·메시지·paho 모듈을 가짜로 넣고 `fleet_adapter`를 읽는다. ROS 없는 시스템 Python(3.12, 임시 venv)에서 직접 돌려 통과 확인 |
| 🔧 CI에서 Mosquitto 띄워 통합 | ✅ 작성 | Go·Python 테스트는 브로커가 필요 없어 서비스 컨테이너는 쓰지 않았다. 대신 `loadtest-smoke` 잡이 apt로 Mosquitto를 깔고 fleetd를 띄워 작은 부하 테스트(로봇 300대, 브라우저 4개 중 느림 1, 끊김 폭풍, `-check`)를 한 번 돈다. 숫자 크기는 보지 않고 기능만(로봇 ONLINE, 브라우저 오류 0, 묶음 받음, 느린 브라우저 때문에 버림·스냅샷 발생, 모두 재접속) 확인한다(7단계 리뷰 7). Linux에서 처음 도는 경로(`/proc` CPU 읽기 포함)라 push 뒤 확인 필요 |
| 부하 3종 비교 | ✅ `cmd/loadtest`, [LOAD_TEST.md](LOAD_TEST.md) | 실제 fleetd(시나리오마다 새로 띄움)에 MQTT 로봇 500대 + WebSocket 브라우저 20개(2개 느림). 묶음 인코딩 한 번 vs 브라우저마다(`-bench-encode-per-client`), 느린 브라우저 버림 vs 기다림(`-bench-block-slow`), 동시 재접속 jitter 있음 vs 없음(끊김 2.5초·3.5초), 브라우저 60개에서 인코딩 비교 한 번 더. 시나리오마다 3번, 중앙값과 범위. 처음 돌렸을 때 찾은 측정 문제: `/live/stats`가 기다리기 모드에서 잠금을 기다리는 동안 다른 카운터를 늦게 읽어 위치 수신이 2,435/s로 적게 나옴 → 로컬 값을 먼저 읽고 stats는 마지막에. jitter 없는 폭풍에서 시도가 왜 7초에 또 오는지 몰라 실패 원인을 기록하게 했더니 접속 대기열 거절("actively refused")이었다 |
| 벤치 스위치가 정말 동작하는지 | ✅ `TestBenchEncodePerClient`, `TestBenchBlockOnSlow`, `TestBenchBlockTimeoutFallsBack` | 스위치가 아무것도 안 하면 부하 테스트가 기본값을 자기 자신과 비교하게 된다. 브라우저 5개면 delta 하나에 인코딩 5번·서로 다른 버퍼, 기다리기는 버림·스냅샷 0번·모든 delta 도착, 시간 초과면 버림으로 돌아감. 각 스위치를 꺼 보는 변이로 실패 확인 |
| 부하 테스트 도구 자체 | ✅ `cmd/loadtest` 단위 5개 | 묶음 머리 읽기(type·time), 백분위·100 ms 칸 최대·히스토그램, 중앙값 요약, jitter 없는 백오프, `-check` 조건(`TestFrameRunCheck`) |
| 7단계 변경 뒤 회귀 | ✅ 수동 | gofmt·`go vet`(Windows·Linux)·`go test ./...`, node 31/31, pytest 74개(pixi ROS 환경 + ROS 없는 시스템 Python 3.12 venv 둘 다), `scripts\e2e.bat` 71/71(`.bat`을 CRLF로 바꾼 뒤). CI 자체는 push 전이라 실행 못 함 |

### 7단계 리뷰 (2026-11-03, 모두 반영)

| # | 지적 | 수정 |
|---|---|---|
| 1 중 (측정됨) | LOAD_TEST가 느린 브라우저는 "항상 최신 상태"를 받는다고 했는데, 잰 나이는 중앙값 3.4초·최대 4.0초 | `cmd/loadtest`가 느린 브라우저가 읽은 delta·스냅샷의 상태 나이를 잼(p50·p99·최대, 받은 스냅샷 수). 다시 잰 값: 버림 3.3초·4.0초, 기다림 7.0초·7.9초. 문장을 고치고, 8칸 대기열 + 소켓 버퍼가 하는 일과 나이를 묶는 방법(최신 하나만, 작은 대기열, 쓰기 버퍼·흐름 제어)을 적음. README 알려진 한계에 추가. fleetd 동작은 그대로 |
| 2 중 | "끊김 길이와 상관없이 약 6초"는 2.5·3.5초 두 값뿐 | "잰 두 끊김(2.5·3.5초)에서 5.7~6.0초", 더 긴 끊김은 재지 않음이라고 씀 |
| 3 중 | 거절 수 "47~188"이 3.5초 표(149 [14–188])와 안 맞음 | 끊김별 중앙값과 범위로: 2.5초 105 [47–181], 3.5초 149 [14–188], 합쳐 14~188 |
| 4 중 | 거절한 것은 Mosquitto가 아니라 부하 테스트의 Go 프록시 listen 소켓 | README·LOAD_TEST에 그렇게 쓰고, 브로커에 직접 붙었을 때의 거절은 재지 않은 추측이라고 표시 |
| 5 낮 | "pose messages in"은 가짜 로봇이 보낸 수 | 이름을 "pose messages sent by the fake robots"로(`report.go`, `PosesSent`) |
| 6 낮 | Go 테스트 함수 수가 틀림 | 명령으로 다시 셈: `go test -list . ./...`의 `Test` 줄 207개(리뷰 뒤 `TestFrameRunCheck` 추가 포함, 그 전 206개), `go test -count=1 -v ./...`의 `--- PASS` 줄 271개(하위 테스트 포함, 맨 앞 줄만 207개). pytest `74 passed`, node `31/31 web tests passed`, e2e `71/71 passed` |
| 7 낮 | CI 스모크가 버림·기다림 경로를 거치지 않고, 묶음 0개나 브라우저 오류로도 통과 | `loadtest -check`: 브라우저 오류(연결된 뒤의 끊김; 503 다시 시도는 제외) > 0, 빠른 브라우저 묶음 0, 느린 브라우저가 있는데 버림 0 또는 받은 스냅샷 0(기다림 시나리오는 버림 조건 제외)이면 실패. CI는 로봇 300대, 느린 브라우저 1초마다 + 받는 버퍼 32 KB(`-slow-rcvbuf`, Linux 소켓 버퍼가 커서), 예열·측정 10초. `TestFrameRunCheck`가 조건마다 실패함을 확인. 이 PC에서 같은 명령 통과(버림 5.6/s, 스냅샷 4). Linux에서는 push 뒤 확인 필요 |
| 8 낮 | 스모크 실패 때 fleetd 로그가 지워짐 | `-keep-logs`, 실패하면 `/tmp/loadtest-fleetd-*/fleetd.log`를 출력하는 단계(`if: failure()`) |
| 9 낮 | `internal/elevator/config.go`의 `TurnTimeout` 주석이 "다음 로봇이 움직인다" | 같은 로봇의 실패한 시도이고 다음 로봇은 `door_zone: 0` 뒤에만이라고 고침 |
| 10 낮 | 액션 버전, 중복 실행, Windows vet | `gh api repos/actions/<name>/releases/latest`: checkout v7.0.1, setup-go v7.0.0, setup-node v7.1.0, setup-python v7.1.0 → 모두 `@v7`. `concurrency`(브랜치/PR마다 하나, 옛 실행 취소). `GOOS=windows go vet ./...` 추가 |
| 11 참고 | `-race` 후보(sqlite checkptr, 시간에 민감한 테스트) | 바꾼 것 없음. `-race`는 checkptr도 켜므로 modernc.org/sqlite 쪽에서 걸릴 수 있다는 점만 기록(로컬에서 확인 불가, CI 첫 실행에서 볼 것) |
| 12 낮 | fleetd `-h`가 형 이름을 빼고 `(default "")`를 찍음 | 숨은 스위치만 뺀 새 FlagSet에 나머지를 옮겨 표준 `PrintDefaults`로 출력 |
| 13 낮 | 시나리오를 차례로 돌려 PC 상태 변화가 한 시나리오에 몰릴 수 있음 | 번갈아(A B C A B C …) 돌림. 묶음 세 시나리오와 브라우저 60개 비교를 다시 잼(LOAD_TEST 표 교체). 재접속 폭풍 표는 그 전 값 그대로(표시함) |
| 14 정보 | 기다림 p99 ≈ 1,001 ms를 "느린 2개 × 0.5초"로 설명한 것은 추측 | 느린 브라우저 1개로 3번 잼: p99 1000.4~1002.1 ms. 그 설명은 틀렸다고 쓰고 원인은 확인 못 함으로 표시 |
| 기타 | 그림 파일 | `.gitattributes`에 `*.png binary`, `*.gif binary` |


---

## 8단계: 이기종 로봇 (Unity)

### 8a: MQTT로 직접 붙는 로봇 (PR #8)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| fleetd `can_ride` (`internal/fleet/registry_test.go`, `internal/task/ride_test.go`) | ✅ 5개 | `TestCanRideDefaultsToTrue`(없음·false·true, 위치만 온 로봇, 상태를 따라 바뀜), `TestRideSkipsRobotThatCannotRide`(더 가까워도 건너뜀), `TestRideCrossFloorTaskWaitsForRider`(못 타는 로봇만 비면 대기, 뒤의 같은 층 작업은 배정, 여러 구간 작업), `TestCannotRideIgnoredWithoutElevators`, `TestRideFlagFlipAfterAssignmentRequeues`(시도 1번·`FailedOn`·이후 명령 없음·다른 로봇이 가져감). `GET /robots`에 `can_ride` |
| Unity 로봇 프로토콜 코어 (EditMode, `Assets/Fleet/Tests`) | ✅ 41개 | `test_fleet_logic.py`의 해당 경우를 옮김: boot·seq, pose 주기·데드밴드·정지 하트비트, ack 수명주기, 같은 `cmd_id`, 만료, cancel(대상 지정), dock·배터리 |
| 실제 fleetd 연동 (ROS 없이) | ✅ 수동 | ONLINE·`can_ride: false`, 작업 DONE, 취소 CANCELLED(goto 응답 `cancelled`), 다른 층 작업은 QUEUED로 남음, 플레이어 강제 종료 → 0.12초에 OFFLINE(LWT), 배터리 20%면 `dock` → CHARGING, 재접속 뒤 구독 복구 |
| 혼합 플릿 (`scripts/mixed_fleet_check.py`) | ✅ 수동 PASS | ROS 로봇 3대(어댑터) + Unity 2대 + mock 엘리베이터: 같은 층 → `unity_1`, 3층 → `robot_2`(엘리베이터로 DONE, 층 3), 취소, Unity 종료 → Unity만 OFFLINE 0.02초·ROS ONLINE 유지 |
| 회귀 | ✅ | `go test ./...`, e2e 71/71(`can_ride` 들어간 브랜치), CI 4개 잡 |

### 8b: 3D 대시보드 (PR #8)

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 묶음 처리·지도 로직 (EditMode `LiveModelTests`, `DashboardMapTests`) | ✅ 38개 (전체 79) | snapshot 교체, delta 적용, `removed` 삭제, seq 빈틈, heartbeat, 시계 차이, 기록 규칙, 주어진 시각의 보간, Go RFC3339(9자리) 시각 읽기, 칸·차 위치, 색 규칙 |
| 실제 화면 (캡처 PNG를 직접 확인) | ✅ 수동 | 혼합 탑승: 칸별 로봇과 내릴 층, 층 사이 차, 4층 문 열림(초록)·하차. STALE(프로세스 멈춤)·OFFLINE(종료), CHARGING, 구독자 64개 꽉 참 → "server full (503)" 뒤 재연결, fleetd 멈춤 → 5초 뒤 재연결 |
| 안 한 것 | — | 마우스 조작 실제로 안 눌러 봄(`-follow`·`-focus`만), ERROR 색은 테스트로만, WebGL 없음, CI에서 Unity 안 돎 |

**8a·8b 리뷰:** 8c와 함께 리뷰했다(아래 8단계 리뷰).

### 8c: Unity 로봇 엘리베이터 탑승 + 대시보드 보정

| 테스트 | 상태 | 리뷰 |
|---|---|---|
| 탑승 로직 (EditMode `RobotRideTests`, `adapter/test_ride_logic.py` 34개를 옮김) | ✅ 43개 | 파이썬 34개(파라미터 경우를 펼침) + 빈 계획. `Core/RobotRide.cs`는 `fleet_logic.py:174-546`을 옮겼고 주석에 줄 범위 |
| 로봇별 끊김·배선 (EditMode `FleetRobotRideTests`) | ✅ 13개 | 한 로봇이 끊긴 동안 그 로봇의 `fleet/*`만 0개(BOARDING 상태·이벤트 제외)·다른 로봇은 계속, 끊긴 동안 받은 명령은 버리고 복귀 뒤 같은 명령은 새것으로, 복귀 순서, 문 구역 시점, 하차 막힘, 갇힘 → 꺼냄, 문 구역에서 고장, `can_ride` false, 토픽 읽기, 움직임 |
| 대시보드 보정 (EditMode `MarkerTweenTests`) | ✅ 6개 | 칸 자리 ↔ 보고된 위치, 층 바뀜에서 0.6초 이어서 옮김, 처음 나타나면 바로 그림. 실제 캡처에서 탑승 로봇이 200 ms 세 칸에 걸쳐 칸으로 들어감 |
| EditMode 전체 | ✅ 142/142 | 기존 79 + 새 63(옛 테스트 2개는 `can_ride` false를 단언해 고쳐 씀) |
| Unity 로봇만 탑승 | ✅ 수동 | `unity_1` → 3층, `unity_2` → 5층 둘 다 DONE, 운행 `E1-ng2vvj-1` 탑승 순서가 계획(unity_2, unity_1)대로, `door_zone` 최대 1, 차 안에서 `fleet/*` 0개(첫 메시지는 하차 `clear` 약 1.0초 뒤) |
| **ROS·Unity 함께 탑승** | ✅ 수동 2회 | 1회(서브에이전트): 운행 `E1-e7gnwo-1`에 unity_1(칸 2)·robot_1(칸 1)·robot_2(칸 0). 2회(직접, 영상 촬영): 운행 `E1-sw7yyk-1`에 unity_2(칸 2, 4층)·robot_2(칸 1, 3층)·robot_1(칸 0, 2층)이 계획 순서대로 탑승·각자 층에 하차, unity_1(5층)은 칸이 3개라 다음 운행. 작업 4개 DONE |
| 하차 막힘 | ✅ 수동 | 한 번 막힘: 3층 하차 `failed`("door zone blocked") → 같은 층 재시도로 DONE. 두 번 막힘: 5층으로 넘겨져 문 쪽이라 먼저 내림 → ride `done` 5층 → 다시 내려가는 운행으로 3층 DONE(ELEVATOR.md 6장대로) |
| 혼합 검증 스크립트 | ✅ PASS 2가지 | 기본: 다른 층 작업을 Unity도 받음(`unity_2` 5층). `--unity-no-ride`(`-canRide false`): ROS만 탐 |
| 회귀 | ✅ | e2e 71/71, 두 플레이어 빌드·실행. fleetd·어댑터·mock·2D 대시보드는 바꾸지 않음 |
| 데모 영상 | ✅ | 함께 타는 3D 대시보드 영상(약 2배속, 51초) |
| 리뷰 | ✅ | 아래 8단계 리뷰 |

### 8단계 리뷰 (2026-11-19, Codex, 8a~8c 전체 `3a23203..`, 모두 수정함)

지적 3개를 코드에서 확인했고 모두 맞았다. 각 수정에 회귀 테스트를 넣고, 수정을 끈 채 한 번 돌려 실패하는지 확인했다(10개 실패, 나머지 4개는 아무것도 안 하는 경우·경계 확인이라 양쪽 다 통과). EditMode 156/156.

| # | 문제 | 수정 | 테스트 |
|---|---|---|---|
| 1 중 | MQTT가 끊긴 동안 Unity 로봇은 QoS 1도 버림. 하차 직후 와이파이 복귀와 MQTT 끊김이 겹치면 `boarded`·`exited` 이벤트를 영영 잃음. ROS 어댑터(paho)는 끊긴 동안의 QoS 1을 붙잡았다가 다시 연결되면 순서대로 보냄 | `Core/PendingQueue.cs`(순서 유지, 최대 1000개, 넘치면 가장 오래된 것을 버리고 셈). 끊긴 동안·발행 실패 뒤 QoS 1을 쌓고, "online" 발행과 구독이 끝난 뒤(`ready`) 새 메시지보다 먼저 보냄. QoS 0(pose)은 버림 | `PendingQueueTests` 6개: 순서·새것보다 먼저, QoS 0 버림, 한도, 실패한 발행은 맨 앞에 남고 멈춤, 실패한 QoS 0, 도중에 끊김. 실제 재연결 확인은 결론을 못 냄(새 구독자가 본 seq가 건너뜀): 단위 테스트에 기댐 |
| 2 중 | 문 구역에서 고장(ERROR)이 나면 움직임은 `running`, 끊김 유지, `reset_error`·`rescue` 명령은 차 안이라 버려짐 → 실행 중인 플레이어로는 풀 수 없음. ROS mock 로봇은 로컬 서비스(현장 사람)가 있음 | 운영자 토픽 `mockrobot/<robot>/operator`(`{"action":"reset_error"}` / `{"action":"rescue"}`), `fleet/*`이 아니므로 끊긴 동안에도 받음(mock 엘리베이터의 `mockelevator/<id>/fault`와 같은 성격). `fleet/*` 응답 없음. ROBOT_PROTOCOL.md 9.1에 적음 | `OperatorResetErrorUnblocksAFaultInTheDoorZoneDuringTheBlackout`, `OperatorRescueTakesARobotOutOfTheCarWhileRiding`, `OperatorRescueOfAStrandedRobotReportsRescued`, `OperatorMessagesThatDoNothingStaySilent` |
| 3 낮 | 대시보드: 하차하면 `/elevators`는 곧 `out`인데 새 pose는 규약상 1초 뒤. 그 사이 탑승 전 층의 기록을 써서 마커가 출발 층 쪽으로 갔다가 하차 층으로 튐(8c 보정이 오히려 눈에 띄게 함) | 차에서 나온 로봇은 그 뒤의 pose가 올 때까지 마지막으로 그린 자리에 둠(최대 5초, 넘으면 기록 사용), 그 뒤 0.6초 보정으로 새 위치로 | `HeldAfterTheCarUntilANewerPoseThenTweensFromTheHeldPosition`, `HoldTimesOutAndFallsBackToTheTrack`, `BackInTheCarEndsTheHold`, `RobotThatWasNeverInACarIsNotHeld`. 실제 캡처에서 하차 뒤 1층 로비에 나타나지 않음 |

**남은 한계:** 3번은 `out`을 본 서버 시각보다 새 pose 샘플을 기다린다. 폴링이 `out`을 보기 전에 pose가 먼저 오고 로봇이 그 뒤 멈춰 있으면 5초 동안 붙잡혀 있다가 기록으로 돌아간다.

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
