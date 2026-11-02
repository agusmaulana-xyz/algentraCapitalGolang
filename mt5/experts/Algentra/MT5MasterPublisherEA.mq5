#property strict
#property version   "1.000"
#property description "Publishes open master positions to the local Algentra copy-trading workspace."

input string SnapshotFileName = "algentra-copy-master.json";
input int PollIntervalMs = 250;

string EscapeJSON(const string value)
  {
   string escaped = "";
   for(int i = 0; i < StringLen(value); i++)
     {
      ushort c = StringGetCharacter(value, i);
      if(c == 92) escaped += "\\\\";
      else if(c == 34) escaped += "\\\"";
      else if(c == 8) escaped += "\\b";
      else if(c == 12) escaped += "\\f";
      else if(c == 10) escaped += "\\n";
      else if(c == 13) escaped += "\\r";
      else if(c == 9) escaped += "\\t";
      else if(c < 0x20) escaped += StringFormat("\\u%04X", c);
      else escaped += StringSubstr(value, i, 1);
     }
   return escaped;
  }

bool WriteSnapshot(const string value)
  {
   string temporary = SnapshotFileName + ".tmp";
   FileDelete(temporary, FILE_COMMON);
   int handle = FileOpen(temporary, FILE_WRITE | FILE_BIN | FILE_COMMON);
   if(handle == INVALID_HANDLE)
     {
      Print("[MT5MasterPublisherEA] cannot open common snapshot file; error ", GetLastError());
      return false;
     }
   char data[];
   int count = StringToCharArray(value, data, 0, WHOLE_ARRAY, CP_UTF8);
   if(count > 0) ArrayResize(data, count - 1);
   uint written = FileWriteArray(handle, data, 0, ArraySize(data));
   FileFlush(handle);
   FileClose(handle);
   if(written != (uint)ArraySize(data))
     {
      FileDelete(temporary, FILE_COMMON);
      Print("[MT5MasterPublisherEA] incomplete common snapshot write");
      return false;
     }
   if(!FileMove(temporary, FILE_COMMON, SnapshotFileName, FILE_COMMON | FILE_REWRITE))
     {
      FileDelete(temporary, FILE_COMMON);
      Print("[MT5MasterPublisherEA] cannot publish common snapshot; error ", GetLastError());
      return false;
     }
   return true;
  }

bool PublishSnapshot()
  {
   if(!TerminalInfoInteger(TERMINAL_CONNECTED) || AccountInfoInteger(ACCOUNT_LOGIN) <= 0)
      return false;

   string body = "{\"active\":true,\"account_login\":\"" +
                 IntegerToString((long)AccountInfoInteger(ACCOUNT_LOGIN)) +
                 "\",\"server\":\"" + EscapeJSON(AccountInfoString(ACCOUNT_SERVER)) +
                 "\",\"updated_at\":" + IntegerToString((long)TimeGMT()) + ",\"positions\":[";
   int total = PositionsTotal();
   int emitted = 0;
   long server_to_gmt = (long)TimeTradeServer() - (long)TimeGMT();
   for(int i = 0; i < total; i++)
     {
      ulong ticket = PositionGetTicket(i);
      if(ticket == 0) continue;
      if(emitted > 0) body += ",";
      long side = PositionGetInteger(POSITION_TYPE);
      body += StringFormat("{\"ticket\":\"%I64u\",\"symbol\":\"%s\",\"action\":\"%s\",\"lots\":%s,\"entry_price\":%s,\"sl\":%s,\"tp\":%s,\"opened_at\":%I64d}",
                           ticket,
                           EscapeJSON(PositionGetString(POSITION_SYMBOL)),
                           side == POSITION_TYPE_BUY ? "BUY" : "SELL",
                           DoubleToString(PositionGetDouble(POSITION_VOLUME), 8),
                           DoubleToString(PositionGetDouble(POSITION_PRICE_OPEN), 10),
                           DoubleToString(PositionGetDouble(POSITION_SL), 10),
                           DoubleToString(PositionGetDouble(POSITION_TP), 10),
                           PositionGetInteger(POSITION_TIME) - server_to_gmt);
      emitted++;
     }
   body += "]}";
   return WriteSnapshot(body);
  }

int OnInit()
  {
   if(StringLen(SnapshotFileName) < 1 || StringFind(SnapshotFileName, "..") >= 0 ||
      StringFind(SnapshotFileName, "\\") >= 0 || StringFind(SnapshotFileName, "/") >= 0 ||
      PollIntervalMs < 50)
      return INIT_PARAMETERS_INCORRECT;
   if(!EventSetMillisecondTimer(PollIntervalMs))
      return INIT_FAILED;
   return INIT_SUCCEEDED;
  }

void OnDeinit(const int reason)
  {
   EventKillTimer();
  }

void OnTimer()
  {
   PublishSnapshot();
  }
