// REST polling: GET /elevators (about once a second, faster while a car has
// a trip so floor changes and riders' boarding/exiting show promptly), /tasks every second and
// /events?n=8 every two seconds for the HUD. UnityWebRequest works in the
// standalone player and in WebGL alike.
using System;
using System.Collections;
using System.Collections.Generic;
using Fleet.Dashboard;
using UnityEngine;
using UnityEngine.Networking;

namespace Fleet.Dashboard3D
{
    public sealed class FleetPoller : MonoBehaviour
    {
        public string Api;
        public List<ElevatorView> Elevators = new List<ElevatorView>();
        public List<TaskView> Tasks = new List<TaskView>();
        public List<FleetEvent> Events = new List<FleetEvent>();
        public bool ApiUp;
        public string ApiError = "";
        public event Action ElevatorsChanged;

        const float TaskPoll = 1f, ElevatorPoll = 1f, ElevatorPollBusy = 0.25f, EventPoll = 2f;

        void Start()
        {
            StartCoroutine(Loop("/elevators", () => AnyBusy() ? ElevatorPollBusy : ElevatorPoll, json =>
            {
                Elevators = Fleet.Dashboard.Elevators.Parse(json);
                ElevatorsChanged?.Invoke();
            }));
            StartCoroutine(Loop("/tasks", () => TaskPoll, json => Tasks = TaskView.Parse(json)));
            StartCoroutine(Loop("/events?n=8", () => EventPoll, json => Events = FleetEvent.Parse(json)));
        }

        bool AnyBusy()
        {
            foreach (var e in Elevators) if (e.Trip != null || (e.State != null && e.State.Moving)) return true;
            return false;
        }

        IEnumerator Loop(string path, Func<float> period, Action<string> onJson)
        {
            while (true)
            {
                using (var req = UnityWebRequest.Get(Api + path))
                {
                    req.timeout = 3;
                    yield return req.SendWebRequest();
                    if (req.result == UnityWebRequest.Result.Success)
                    {
                        try
                        {
                            onJson(req.downloadHandler.text);
                            ApiUp = true;
                            ApiError = "";
                        }
                        catch (Exception e)
                        {
                            ApiError = $"{path}: {e.Message}";
                            Debug.LogWarning($"[dash] {ApiError}");
                        }
                    }
                    else
                    {
                        ApiUp = false;
                        ApiError = $"{path}: {req.error}";
                    }
                }
                yield return new WaitForSecondsRealtime(period());
            }
        }
    }
}
