// Port of fleet_logic.CommandBook. QoS 1 can deliver a command twice and
// fleetd re-sends when an ack is lost: a duplicate must not run again but must
// get the last ack again, or fleetd times out a command that is running.
using System.Collections.Generic;

namespace Fleet.Core
{
    public enum Verdict { New, Duplicate, Expired }

    public sealed class CommandBook
    {
        readonly int size;
        readonly Dictionary<string, string> acks = new Dictionary<string, string>(); // cmd_id -> last ack JSON or null
        readonly LinkedList<string> order = new LinkedList<string>();               // oldest first

        public CommandBook(int size = Protocol.CommandMemory) { this.size = size; }

        /// <summary>The id is remembered before the expiry test, so a re-sent
        /// expired command is a duplicate and gets the same "expired" answer.</summary>
        public Verdict Check(string cmdId, double? expiresAt, double nowWall)
        {
            if (acks.ContainsKey(cmdId))
                return Verdict.Duplicate;
            acks[cmdId] = null;
            order.AddLast(cmdId);
            while (order.Count > size)
            {
                acks.Remove(order.First.Value);
                order.RemoveFirst();
            }
            if (expiresAt.HasValue && nowWall > expiresAt.Value)
                return Verdict.Expired;
            return Verdict.New;
        }

        public void Record(string cmdId, string ackJson)
        {
            if (acks.ContainsKey(cmdId))
                acks[cmdId] = ackJson;
        }

        /// <summary>The ack to repeat for a duplicate, or null if none was sent yet.</summary>
        public string LastAck(string cmdId) => acks.TryGetValue(cmdId, out var a) ? a : null;
    }
}
