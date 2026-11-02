#property strict
#property version   "1.010"
#property description "Verifies the MT5 login on the broker-specific XAUUSD chart."

input string ProbeStatusFileName = "algentra-login-probe.json";
input string ExpectedGoldSymbol = "XAUUSD";

bool g_status_write_error_logged = false;
string g_last_status_log = "";

string EscapeJSON(string value)
  {
   StringReplace(value, "\\", "\\\\");
   StringReplace(value, "\"", "\\\"");
   StringReplace(value, "\r", " ");
   StringReplace(value, "\n", " ");
   return value;
  }

void WriteStatusToFile(const string file_name,
                       const bool ready,
                       const bool valid,
                       const string message,
                       const string symbol,
                       const bool common)
  {
   int flags = FILE_WRITE|FILE_TXT|FILE_ANSI|FILE_SHARE_READ|FILE_SHARE_WRITE;
   if(common)
      flags |= FILE_COMMON;
   int file = FileOpen(file_name, flags);
   if(file == INVALID_HANDLE)
     {
      if(!g_status_write_error_logged)
        {
         Print("[MT5LoginProbe] cannot write ", common ? "common" : "portable",
               " status file ", file_name, "; error ", GetLastError());
         g_status_write_error_logged = true;
        }
      return;
     }
   g_status_write_error_logged = false;

   string safe_message = EscapeJSON(message);
   string safe_symbol = EscapeJSON(symbol);
   string server = EscapeJSON(AccountInfoString(ACCOUNT_SERVER));
   string currency = EscapeJSON(AccountInfoString(ACCOUNT_CURRENCY));
   string balance = DoubleToString(AccountInfoDouble(ACCOUNT_BALANCE), 8);
   string chart_symbol = EscapeJSON(_Symbol);
   FileWriteString(file, StringFormat("{\"ready\":%s,\"valid\":%s,\"message\":\"%s\",\"login\":%I64d,\"server\":\"%s\",\"symbol\":\"%s\",\"chart_symbol\":\"%s\",\"balance\":%s,\"currency\":\"%s\"}",
                                      ready ? "true" : "false",
                                      valid ? "true" : "false",
                                      safe_message,
                                      AccountInfoInteger(ACCOUNT_LOGIN),
                                      server,
                                      safe_symbol,
                                      chart_symbol,
                                      balance,
                                      currency));
   FileFlush(file);
   FileClose(file);
  }

void WriteStatus(const bool ready, const bool valid, const string message, const string symbol)
  {
   string safe_message = EscapeJSON(message);
   string safe_symbol = EscapeJSON(symbol);
   string server = EscapeJSON(AccountInfoString(ACCOUNT_SERVER));
   string chart_symbol = EscapeJSON(_Symbol);
   string currency = EscapeJSON(AccountInfoString(ACCOUNT_CURRENCY));
   string balance = DoubleToString(AccountInfoDouble(ACCOUNT_BALANCE), 8);
   string payload = StringFormat("{\"ready\":%s,\"valid\":%s,\"message\":\"%s\",\"login\":%I64d,\"server\":\"%s\",\"symbol\":\"%s\",\"chart_symbol\":\"%s\",\"balance\":%s,\"currency\":\"%s\"}",
                                  ready ? "true" : "false",
                                  valid ? "true" : "false",
                                  safe_message,
                                  AccountInfoInteger(ACCOUNT_LOGIN),
                                  server,
                                  safe_symbol,
                                  chart_symbol,
                                  balance,
                                  currency);
   if(payload != g_last_status_log)
     {
      Print("[MT5LoginProbe] STATUS ", payload);
      g_last_status_log = payload;
     }
   WriteStatusToFile(ProbeStatusFileName, ready, valid, message, symbol, false);
   WriteStatusToFile(ProbeStatusFileName, ready, valid, message, symbol, true);
  }

int OnInit()
  {
   g_status_write_error_logged = false;
   g_last_status_log = "";
   WriteStatus(false, false, "Menunggu koneksi broker MT5.", ExpectedGoldSymbol);
   if(!EventSetTimer(1))
      return INIT_FAILED;
   return INIT_SUCCEEDED;
  }

void OnDeinit(const int reason)
  {
   EventKillTimer();
  }

string ResolveGoldSymbol(const string requested)
  {
   string normalized = requested;
   StringToUpper(normalized);
   if(StringLen(normalized) >= 6 && StringSubstr(normalized, 0, 6) == "XAUUSD" &&
      SymbolSelect(requested, true))
      return requested;

   string best_match = "";
   int total = SymbolsTotal(false);
   for(int i = 0; i < total; i++)
     {
      string candidate = SymbolName(i, false);
      string candidate_base = StringSubstr(candidate, 0, 6);
      StringToUpper(candidate_base);
      if(StringLen(candidate) < 6 || candidate_base != "XAUUSD")
         continue;

      string candidate_upper = candidate;
      StringToUpper(candidate_upper);
      if(StringLen(normalized) > 6 && candidate_upper == normalized && SymbolSelect(candidate, true))
         return candidate;
      if(best_match == "" && SymbolSelect(candidate, true))
         best_match = candidate;
     }

   if(best_match != "")
      return best_match;
   return "";
  }

void OnTimer()
  {
   if(!TerminalInfoInteger(TERMINAL_CONNECTED) || AccountInfoInteger(ACCOUNT_LOGIN) <= 0)
     {
      WriteStatus(false, false, "Menunggu koneksi broker MT5.", ExpectedGoldSymbol);
      return;
     }

   string target_symbol = ResolveGoldSymbol(ExpectedGoldSymbol);
   if(target_symbol == "")
     {
      WriteStatus(false, false, "Menunggu simbol XAUUSD broker tersedia di daftar MT5.", "");
      return;
     }

   if(StringCompare(_Symbol, target_symbol, false) != 0 || _Period != PERIOD_M1)
     {
      ResetLastError();
      if(!ChartSetSymbolPeriod(0, target_symbol, PERIOD_M1))
        {
         string message = StringFormat("Chart %s belum dapat dialihkan ke %s,M1; error %d.",
                                       _Symbol, target_symbol, GetLastError());
         WriteStatus(false, false, message, target_symbol);
         return;
        }
      WriteStatus(false, false, "Menyiapkan chart broker " + target_symbol + ",M1.", target_symbol);
      return;
     }

   string message = "Login broker berhasil. EA aktif pada chart " + target_symbol + ",M1.";
   Print("[MT5LoginProbe] ", message);
   WriteStatus(true, true, message, target_symbol);
   EventKillTimer();
  }
