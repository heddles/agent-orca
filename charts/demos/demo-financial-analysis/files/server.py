#!/usr/bin/env python3
"""
financial-mcp-server: MCP HTTP transport server for the demo-financial-analysis demo.

Exposes two tools:
  forecast_impact(scenario, ...)       -- multi-factor financial forecast; app: interactive dashboard
  fetch_market_context(commodities)    -- current market baselines; app: market data table

All JSON-RPC 2.0 requests arrive as POST / with Content-Type: application/json.
Responses are returned synchronously (HTTP transport, not SSE).
"""
import json, math, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── Synthetic data ───────────────────────────────────────────────────────────

# Deterministic-ish seed so data looks stable across tool calls in the same hour.
_seed = int(time.time()) // 3600


def _rng(name, offset=0):
    """Seeded pseudo-random float in [0,1] for a given key."""
    h = hash(name + str(_seed + offset)) & 0xFFFFFF
    return h / 0xFFFFFF


# Baseline commodity data (realistic mid-2020s values).
COMMODITIES = {
    "wheat":      {"price": 6.50,   "unit": "$/bushel",  "high52": 8.20,  "low52": 5.10,  "vol30": 18.5, "trend": "flat"},
    "corn":       {"price": 4.80,   "unit": "$/bushel",  "high52": 6.10,  "low52": 4.00,  "vol30": 16.2, "trend": "down"},
    "soybeans":   {"price": 13.20,  "unit": "$/bushel",  "high52": 15.80, "low52": 11.50, "vol30": 14.8, "trend": "flat"},
    "crude_oil":  {"price": 78.00,  "unit": "$/barrel",  "high52": 93.00, "low52": 64.00, "vol30": 22.1, "trend": "up"},
    "gold":       {"price": 2050.0, "unit": "$/oz",      "high52": 2450.0,"low52": 1820.0,"vol30": 12.3, "trend": "up"},
    "silver":     {"price": 24.50,  "unit": "$/oz",      "high52": 31.00, "low52": 20.50, "vol30": 24.6, "trend": "up"},
    "usd_index":  {"price": 104.0,  "unit": "index",     "high52": 110.0, "low52": 98.0,  "vol30": 6.8,  "trend": "flat"},
    "us_10yr":    {"price": 4.25,   "unit": "% yield",   "high52": 5.00,  "low52": 3.30,  "vol30": 8.2,  "trend": "flat"},
}

# Pairwise correlations (symmetric; stored as sorted tuple keys).
CORRELATIONS = {
    ("corn", "wheat"): 0.85, ("soybeans", "wheat"): 0.65, ("corn", "soybeans"): 0.72,
    ("crude_oil", "wheat"): 0.45, ("corn", "crude_oil"): 0.55,
    ("gold", "wheat"): 0.35, ("crude_oil", "gold"): 0.40, ("gold", "silver"): 0.88,
    ("gold", "usd_index"): -0.65, ("gold", "us_10yr"): -0.55,
    ("crude_oil", "usd_index"): -0.30, ("silver", "wheat"): 0.30,
}

# Which assets to show per primary commodity (always include gold + usd_index).
RELATED_ASSETS = {
    "wheat":     ["wheat", "corn", "crude_oil", "gold", "usd_index"],
    "corn":      ["corn", "wheat", "soybeans", "gold", "usd_index"],
    "soybeans":  ["soybeans", "corn", "wheat", "gold", "usd_index"],
    "crude_oil": ["crude_oil", "wheat", "corn", "gold", "usd_index"],
    "gold":      ["gold", "wheat", "crude_oil", "silver", "usd_index"],
    "silver":    ["silver", "gold", "wheat", "crude_oil", "usd_index"],
}

# Factor sensitivity matrix: how each input factor affects each asset.
# Values are elasticities: a 1-unit change in the factor produces this % change in the asset.
BASE_SENSITIVITIES = {
    "supply_shock_severity": {
        "wheat": 0.85, "corn": 0.60, "soybeans": 0.40, "crude_oil": 0.15,
        "gold": 0.25, "silver": 0.22, "usd_index": -0.10, "us_10yr": 0.08,
    },
    "disruption_months": {
        "wheat": 0.50, "corn": 0.35, "soybeans": 0.25, "crude_oil": 0.10,
        "gold": 0.18, "silver": 0.15, "usd_index": -0.08, "us_10yr": 0.05,
    },
    "demand_growth_rate": {
        "wheat": 0.30, "corn": 0.35, "soybeans": 0.30, "crude_oil": 0.45,
        "gold": 0.20, "silver": 0.25, "usd_index": 0.10, "us_10yr": 0.15,
    },
    "central_bank_stance": {
        "wheat": -0.10, "corn": -0.10, "soybeans": -0.08, "crude_oil": -0.15,
        "gold": -0.55, "silver": -0.50, "usd_index": 0.40, "us_10yr": 0.30,
    },
    "geopolitical_risk": {
        "wheat": 0.30, "corn": 0.20, "soybeans": 0.15, "crude_oil": 0.50,
        "gold": 0.45, "silver": 0.38, "usd_index": 0.15, "us_10yr": -0.10,
    },
}


def _get_corr(a, b):
    """Look up correlation, handling key ordering."""
    if a == b:
        return 1.0
    key = tuple(sorted([a, b]))
    return CORRELATIONS.get(key, 0.0)


def _stance_value(stance):
    """Map central bank stance to numeric value for calculations."""
    return {"dovish": -30, "neutral": 0, "hawkish": 30}.get(stance, 0)


def forecast_impact(args):
    """Generate a multi-factor financial impact forecast."""
    scenario = args.get("scenario", "Unspecified scenario")
    primary = args.get("primary_commodity", "wheat")
    if primary not in COMMODITIES:
        primary = "wheat"

    severity = float(args.get("supply_shock_severity", 50))
    duration = int(args.get("disruption_months", 12))
    demand = float(args.get("demand_growth_rate", 2.0))
    stance = args.get("central_bank_stance", "neutral")
    geo_risk = float(args.get("geopolitical_risk", 30))

    assets_list = RELATED_ASSETS.get(primary, RELATED_ASSETS["wheat"])
    quarters = 40  # 10 years

    # Build input parameters dict (echoed back for iframe initialization).
    input_params = {
        "supply_shock_severity": severity,
        "disruption_months": duration,
        "demand_growth_rate": demand,
        "central_bank_stance": stance,
        "geopolitical_risk": geo_risk,
    }

    # Factor values normalized to [0,1] range for sensitivity calculation.
    factor_vals = {
        "supply_shock_severity": severity / 100.0,
        "disruption_months": duration / 60.0,
        "demand_growth_rate": (demand + 5) / 15.0,  # -5..10 -> 0..1
        "central_bank_stance": (_stance_value(stance) + 30) / 60.0,  # -30..30 -> 0..1
        "geopolitical_risk": geo_risk / 100.0,
    }

    assets_data = {}
    for asset in assets_list:
        base = COMMODITIES[asset]
        base_price = base["price"]

        # Compute total shock multiplier from all factors.
        total_mult = 0.0
        for factor, fval in factor_vals.items():
            sens = BASE_SENSITIVITIES[factor].get(asset, 0.0)
            total_mult += sens * fval

        # Correlation dampening: non-primary assets get dampened impact.
        corr = _get_corr(primary, asset)
        if asset != primary:
            total_mult *= abs(corr)

        # Build quarterly trajectory with decay and widening confidence bands.
        trajectory = []
        now = datetime.now(timezone.utc)
        year = now.year
        q = (now.month - 1) // 3

        for i in range(quarters):
            qi = q + i
            yr = year + qi // 4
            qn = (qi % 4) + 1

            # Shock decays over time (half-life ~ disruption duration in quarters).
            half_life_q = max(duration / 3.0, 2.0)
            decay = math.exp(-0.693 * i / half_life_q)

            # Demand growth accumulates over time.
            demand_accum = (demand / 100.0) * (i / 4.0)

            # Geopolitical premium decays faster.
            geo_decay = math.exp(-0.693 * i / max(half_life_q * 0.5, 1.5))
            geo_premium = (geo_risk / 100.0) * 0.15 * geo_decay * abs(corr if asset != primary else 1.0)

            # Policy effect grows then stabilizes.
            policy_effect = (_stance_value(stance) / 100.0) * min(i / 8.0, 1.0)
            if asset in ("gold", "silver"):
                policy_effect *= -1.5  # Gold moves inversely to hawkish policy
            elif asset == "usd_index":
                policy_effect *= 1.2  # USD strengthens with hawkish policy

            shock_component = total_mult * decay
            p50_mult = 1.0 + shock_component + demand_accum + geo_premium + policy_effect

            # Confidence bands widen with sqrt(time).
            spread = 0.02 * math.sqrt(i + 1) * (1.0 + abs(total_mult))
            # Add some pseudo-random jitter for realism.
            jitter = (_rng(f"{asset}-{i}", offset=1) - 0.5) * 0.005 * (i + 1)

            p50 = round(base_price * (p50_mult + jitter), 2)
            p10 = round(base_price * (p50_mult - spread + jitter * 0.5), 2)
            p90 = round(base_price * (p50_mult + spread + jitter * 0.5), 2)

            trajectory.append({
                "quarter": f"Q{qn} {yr}",
                "p10": p10, "p50": p50, "p90": p90,
            })

        # Summary projections at key horizons.
        projections = {}
        for label, idx in [("1Y", 3), ("2Y", 7), ("5Y", 19), ("10Y", 39)]:
            t = trajectory[min(idx, len(trajectory) - 1)]
            projections[label] = {"p10": t["p10"], "p50": t["p50"], "p90": t["p90"]}

        assets_data[asset] = {
            "baseline_price": base_price,
            "unit": base["unit"],
            "projections": projections,
            "trajectory": trajectory,
        }

    # Build factor sensitivities subset for displayed assets.
    sens_subset = {}
    for factor in BASE_SENSITIVITIES:
        sens_subset[factor] = {}
        for asset in assets_list:
            sens_subset[factor][asset] = BASE_SENSITIVITIES[factor].get(asset, 0.0)

    # Build correlations subset.
    corr_subset = {}
    for a in assets_list:
        corr_subset[a] = {}
        for b in assets_list:
            corr_subset[a][b] = _get_corr(a, b)

    # Generate narrative summary.
    p = assets_data.get(primary, {})
    g = assets_data.get("gold", {})
    primary_5y = p.get("projections", {}).get("5Y", {})
    gold_5y = g.get("projections", {}).get("5Y", {})
    base_p = COMMODITIES[primary]["price"]
    base_g = COMMODITIES["gold"]["price"]
    pct_primary = round((primary_5y.get("p50", base_p) / base_p - 1) * 100, 1) if base_p else 0
    pct_gold = round((gold_5y.get("p50", base_g) / base_g - 1) * 100, 1) if base_g else 0

    narrative = (
        f"A {severity:.0f}% supply shock to {primary} over {duration} months "
        f"with {stance} monetary policy and {geo_risk:.0f}/100 geopolitical risk "
        f"projects {primary} at {primary_5y.get('p50', 'N/A')} {COMMODITIES[primary]['unit']} "
        f"({pct_primary:+.1f}%) and gold at {gold_5y.get('p50', 'N/A')} $/oz "
        f"({pct_gold:+.1f}%) at the 5-year horizon (P50 estimate). "
        f"The P10-P90 range for {primary} at 5Y is "
        f"{primary_5y.get('p10', 'N/A')}-{primary_5y.get('p90', 'N/A')} {COMMODITIES[primary]['unit']}."
    )

    return {
        "scenario": scenario,
        "analysis_timestamp": datetime.now(timezone.utc).isoformat(),
        "primary_commodity": primary,
        "time_horizons": ["1Y", "2Y", "5Y", "10Y"],
        "assets": assets_data,
        "factor_sensitivities": sens_subset,
        "input_parameters": input_params,
        "correlations": corr_subset,
        "narrative": narrative,
    }


def fetch_market_context(args):
    """Return current market data for requested commodities."""
    requested = args.get("commodities", list(COMMODITIES.keys()))
    result = {}
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    for name in requested:
        if name in COMMODITIES:
            c = COMMODITIES[name]
            # Add small deterministic jitter to current price for realism.
            jitter = (_rng(name) - 0.5) * c["price"] * 0.01
            result[name] = {
                "current_price": round(c["price"] + jitter, 2),
                "unit": c["unit"],
                "52w_high": c["high52"],
                "52w_low": c["low52"],
                "volatility_30d": c["vol30"],
                "trend": c["trend"],
            }
        else:
            result[name] = {"error": f"Unknown commodity: {name}"}
    return {"commodities": result, "last_updated": ts}


# ── HTML Apps ────────────────────────────────────────────────────────────────


def forecast_dashboard_html():
    """Self-contained interactive forecast dashboard with Canvas chart and sliders."""
    return '''<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Financial Impact Forecast</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f172a;color:#e2e8f0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:16px;font-size:13px}
h1{font-size:18px;font-weight:600;color:#f8fafc;margin-bottom:2px}
.sub{font-size:12px;color:#94a3b8;margin-bottom:16px}
.controls{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:10px;margin-bottom:16px;padding:12px;background:#1e293b;border-radius:8px;border:1px solid #334155}
.ctrl-group{display:flex;flex-direction:column;gap:3px}
.ctrl-group label{font-size:11px;color:#94a3b8;font-weight:500;display:flex;justify-content:space-between;align-items:center}
.ctrl-group label .val{color:#38bdf8;font-weight:600;font-size:12px}
input[type=range]{width:100%;accent-color:#38bdf8;height:6px;cursor:pointer}
.stance-btns{display:flex;gap:4px}
.stance-btn{flex:1;padding:4px 8px;border:1px solid #475569;border-radius:4px;background:#1e293b;color:#94a3b8;font-size:11px;cursor:pointer;text-align:center;transition:all .15s}
.stance-btn.active{background:#38bdf8;color:#0f172a;border-color:#38bdf8;font-weight:600}
.chart-wrap{position:relative;background:#1e293b;border-radius:8px;border:1px solid #334155;padding:12px;margin-bottom:16px}
canvas{width:100%;display:block;border-radius:4px}
.legend{display:flex;gap:16px;flex-wrap:wrap;margin-top:8px}
.legend-item{display:flex;align-items:center;gap:5px;font-size:11px;cursor:pointer;opacity:1;transition:opacity .15s}
.legend-item.hidden{opacity:0.3}
.legend-dot{width:10px;height:10px;border-radius:50%;flex-shrink:0}
.section-title{font-size:13px;font-weight:600;color:#f8fafc;margin-bottom:8px}
.heatmap{width:100%;border-collapse:collapse;margin-bottom:16px;font-size:11px}
.heatmap th{padding:6px 8px;text-align:left;color:#94a3b8;font-weight:500;border-bottom:1px solid #334155}
.heatmap td{padding:6px 8px;text-align:center;border-bottom:1px solid #1e293b}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:10px;margin-bottom:8px}
.card{background:#1e293b;border-radius:8px;padding:12px;border:1px solid #334155}
.card .name{font-size:11px;color:#94a3b8;margin-bottom:4px;text-transform:uppercase;letter-spacing:0.5px}
.card .price{font-size:18px;font-weight:700;margin-bottom:2px}
.card .change{font-size:12px;font-weight:600}
.card .range{font-size:10px;color:#64748b;margin-top:4px}
.up{color:#4ade80}.down{color:#f87171}.flat{color:#94a3b8}
.tooltip{position:absolute;pointer-events:none;background:#0f172a;border:1px solid #475569;border-radius:6px;padding:8px 10px;font-size:11px;display:none;z-index:10;white-space:nowrap}
.waiting{text-align:center;padding:60px;color:#64748b;font-size:14px}
</style>
</head>
<body>
<h1>Financial Impact Forecast</h1>
<div class="sub" id="subtitle">Waiting for forecast data...</div>

<div id="waiting" class="waiting">Run the forecast-impact tool to populate this dashboard.</div>

<div id="dashboard" style="display:none">
<div class="controls" id="controls">
  <div class="ctrl-group">
    <label>Supply Shock Severity <span class="val" id="v-shock">50%</span></label>
    <input type="range" id="s-shock" min="0" max="100" value="50" step="1">
  </div>
  <div class="ctrl-group">
    <label>Disruption Duration <span class="val" id="v-duration">12 mo</span></label>
    <input type="range" id="s-duration" min="1" max="60" value="12" step="1">
  </div>
  <div class="ctrl-group">
    <label>Demand Growth Rate <span class="val" id="v-demand">2.0%</span></label>
    <input type="range" id="s-demand" min="-50" max="100" value="20" step="1">
  </div>
  <div class="ctrl-group">
    <label>Geopolitical Risk <span class="val" id="v-geo">30</span></label>
    <input type="range" id="s-geo" min="0" max="100" value="30" step="1">
  </div>
  <div class="ctrl-group">
    <label>Central Bank Stance</label>
    <div class="stance-btns">
      <div class="stance-btn" data-stance="dovish" id="btn-dovish">Dovish</div>
      <div class="stance-btn active" data-stance="neutral" id="btn-neutral">Neutral</div>
      <div class="stance-btn" data-stance="hawkish" id="btn-hawkish">Hawkish</div>
    </div>
  </div>
</div>

<div class="chart-wrap">
  <div class="section-title">Projected Price Trajectory (% Change from Baseline)</div>
  <canvas id="chart" height="320"></canvas>
  <div class="tooltip" id="tooltip"></div>
  <div class="legend" id="legend"></div>
</div>

<div class="section-title">Factor Sensitivity Matrix</div>
<table class="heatmap" id="heatmap"><thead><tr><th>Factor</th></tr></thead><tbody></tbody></table>

<div class="section-title">5-Year Outlook</div>
<div class="cards" id="cards"></div>
</div>

<script>
(function(){
var DATA=null,ASSETS=[],VISIBLE={},STANCE="neutral";
var COLORS={"wheat":"#f59e0b","corn":"#22c55e","soybeans":"#a78bfa","crude_oil":"#3b82f6","gold":"#eab308","silver":"#94a3b8","usd_index":"#64748b","us_10yr":"#ec4899"};
var FACTOR_LABELS={"supply_shock_severity":"Supply Shock","disruption_months":"Duration","demand_growth_rate":"Demand Growth","central_bank_stance":"Central Bank","geopolitical_risk":"Geo Risk"};
var STANCE_MAP={"dovish":-30,"neutral":0,"hawkish":30};

function getSliders(){
  return{
    supply_shock_severity:+document.getElementById("s-shock").value,
    disruption_months:+document.getElementById("s-duration").value,
    demand_growth_rate:+document.getElementById("s-demand").value/10,
    central_bank_stance:STANCE,
    geopolitical_risk:+document.getElementById("s-geo").value
  };
}

function factorNorm(name,val){
  if(name==="supply_shock_severity")return val/100;
  if(name==="disruption_months")return val/60;
  if(name==="demand_growth_rate")return(val+5)/15;
  if(name==="central_bank_stance")return(STANCE_MAP[val]+30)/60;
  if(name==="geopolitical_risk")return val/100;
  return 0;
}
function origNorm(name){
  var ip=DATA.input_parameters;
  if(name==="supply_shock_severity")return ip.supply_shock_severity/100;
  if(name==="disruption_months")return ip.disruption_months/60;
  if(name==="demand_growth_rate")return(ip.demand_growth_rate+5)/15;
  if(name==="central_bank_stance")return(STANCE_MAP[ip.central_bank_stance]+30)/60;
  if(name==="geopolitical_risk")return ip.geopolitical_risk/100;
  return 0;
}

function recalcTrajectory(asset){
  var base=DATA.assets[asset];
  if(!base)return[];
  var orig=base.trajectory;
  var sv=getSliders();
  var sens=DATA.factor_sensitivities;
  var factors=Object.keys(sens);
  var out=[];
  for(var i=0;i<orig.length;i++){
    var ratio=1;
    for(var f=0;f<factors.length;f++){
      var fn=factors[f];
      var s=sens[fn][asset]||0;
      var curN=factorNorm(fn,fn==="central_bank_stance"?sv[fn]:sv[fn]);
      var origN=origNorm(fn);
      var delta=curN-origN;
      ratio*=(1+s*delta);
    }
    out.push({
      quarter:orig[i].quarter,
      p10:orig[i].p10*ratio,
      p50:orig[i].p50*ratio,
      p90:orig[i].p90*ratio
    });
  }
  return out;
}

function drawChart(){
  if(!DATA)return;
  var canvas=document.getElementById("chart");
  var dpr=window.devicePixelRatio||1;
  var rect=canvas.getBoundingClientRect();
  canvas.width=rect.width*dpr;
  canvas.height=320*dpr;
  var ctx=canvas.getContext("2d");
  ctx.scale(dpr,dpr);
  var W=rect.width,H=320;
  var pad={t:20,r:16,b:40,l:56};
  var cw=W-pad.l-pad.r,ch=H-pad.t-pad.b;

  ctx.clearRect(0,0,W,H);

  // Compute all trajectories as % change from baseline.
  var series={};
  var minY=Infinity,maxY=-Infinity;
  for(var a=0;a<ASSETS.length;a++){
    var asset=ASSETS[a];
    if(!VISIBLE[asset])continue;
    var traj=recalcTrajectory(asset);
    var baseP=DATA.assets[asset].baseline_price;
    var pcts=traj.map(function(t){return((t.p50/baseP)-1)*100;});
    var p10s=traj.map(function(t){return((t.p10/baseP)-1)*100;});
    var p90s=traj.map(function(t){return((t.p90/baseP)-1)*100;});
    series[asset]={pcts:pcts,p10s:p10s,p90s:p90s,labels:traj.map(function(t){return t.quarter;})};
    for(var i=0;i<pcts.length;i++){
      if(p10s[i]<minY)minY=p10s[i];
      if(p90s[i]>maxY)maxY=p90s[i];
    }
  }
  if(minY===Infinity){minY=-10;maxY=10;}
  var rangeY=maxY-minY||1;
  minY-=rangeY*0.08;maxY+=rangeY*0.08;
  rangeY=maxY-minY;

  var n=40;
  function x(i){return pad.l+i/(n-1)*cw;}
  function y(v){return pad.t+(1-(v-minY)/rangeY)*ch;}

  // Grid lines.
  ctx.strokeStyle="#1e293b";ctx.lineWidth=1;
  var gridStep=Math.pow(10,Math.floor(Math.log10(rangeY/4)))*Math.ceil(rangeY/4/Math.pow(10,Math.floor(Math.log10(rangeY/4))));
  if(gridStep<1)gridStep=1;
  var gs=Math.ceil(minY/gridStep)*gridStep;
  ctx.font="10px -apple-system,sans-serif";ctx.fillStyle="#64748b";ctx.textAlign="right";
  for(var gv=gs;gv<=maxY;gv+=gridStep){
    var gy=y(gv);
    ctx.beginPath();ctx.moveTo(pad.l,gy);ctx.lineTo(W-pad.r,gy);ctx.stroke();
    ctx.fillText(gv.toFixed(1)+"%",pad.l-6,gy+3);
  }

  // Zero line.
  if(minY<=0&&maxY>=0){
    ctx.strokeStyle="#475569";ctx.lineWidth=1;ctx.setLineDash([4,4]);
    ctx.beginPath();ctx.moveTo(pad.l,y(0));ctx.lineTo(W-pad.r,y(0));ctx.stroke();
    ctx.setLineDash([]);
  }

  // X-axis labels (every 2 years = 8 quarters).
  ctx.textAlign="center";ctx.fillStyle="#64748b";
  var firstSeries=series[Object.keys(series)[0]];
  if(firstSeries){
    for(var i=0;i<n;i+=8){
      ctx.fillText(firstSeries.labels[i]||"",x(i),H-pad.b+18);
    }
    ctx.fillText(firstSeries.labels[n-1]||"",x(n-1),H-pad.b+18);
  }

  // Draw bands and lines for each asset.
  for(var a=0;a<ASSETS.length;a++){
    var asset=ASSETS[a];
    if(!VISIBLE[asset]||!series[asset])continue;
    var s=series[asset];
    var color=COLORS[asset]||"#94a3b8";

    // P10-P90 band.
    ctx.fillStyle=color.replace(")",",0.10)").replace("rgb","rgba");
    if(color.charAt(0)==="#"){
      var r=parseInt(color.slice(1,3),16),g=parseInt(color.slice(3,5),16),b=parseInt(color.slice(5,7),16);
      ctx.fillStyle="rgba("+r+","+g+","+b+",0.12)";
    }
    ctx.beginPath();
    for(var i=0;i<n;i++){ctx.lineTo(x(i),y(s.p90s[i]));}
    for(var i=n-1;i>=0;i--){ctx.lineTo(x(i),y(s.p10s[i]));}
    ctx.closePath();ctx.fill();

    // P50 line.
    ctx.strokeStyle=color;ctx.lineWidth=2;
    ctx.beginPath();
    for(var i=0;i<n;i++){
      if(i===0)ctx.moveTo(x(i),y(s.pcts[i]));
      else ctx.lineTo(x(i),y(s.pcts[i]));
    }
    ctx.stroke();
  }

  // Store for tooltip.
  window._chartMeta={series:series,x:x,y:y,pad:pad,W:W,H:H,n:n,cw:cw};
}

function updateLabels(){
  var sv=getSliders();
  document.getElementById("v-shock").textContent=sv.supply_shock_severity+"%";
  document.getElementById("v-duration").textContent=sv.disruption_months+" mo";
  document.getElementById("v-demand").textContent=sv.demand_growth_rate.toFixed(1)+"%";
  document.getElementById("v-geo").textContent=sv.geopolitical_risk;
}

function updateHeatmap(){
  if(!DATA)return;
  var thead=document.querySelector("#heatmap thead tr");
  var tbody=document.querySelector("#heatmap tbody");
  thead.innerHTML="<th>Factor</th>";
  for(var a=0;a<ASSETS.length;a++){
    thead.innerHTML+="<th>"+ASSETS[a].replace("_"," ")+"</th>";
  }
  tbody.innerHTML="";
  var sens=DATA.factor_sensitivities;
  var factors=Object.keys(sens);
  for(var f=0;f<factors.length;f++){
    var fn=factors[f];
    var row="<tr><td style='text-align:left;color:#cbd5e1;font-weight:500'>"+(FACTOR_LABELS[fn]||fn)+"</td>";
    for(var a=0;a<ASSETS.length;a++){
      var v=sens[fn][ASSETS[a]]||0;
      var absV=Math.abs(v);
      var intensity=Math.min(absV/0.85,1);
      var bg;
      if(v>0)bg="rgba(74,222,128,"+intensity*0.4+")";
      else if(v<0)bg="rgba(248,113,113,"+intensity*0.4+")";
      else bg="transparent";
      row+="<td style='background:"+bg+"'>"+v.toFixed(2)+"</td>";
    }
    row+="</tr>";
    tbody.innerHTML+=row;
  }
}

function updateCards(){
  if(!DATA)return;
  var container=document.getElementById("cards");
  container.innerHTML="";
  for(var a=0;a<ASSETS.length;a++){
    var asset=ASSETS[a];
    var traj=recalcTrajectory(asset);
    var base=DATA.assets[asset].baseline_price;
    var unit=DATA.assets[asset].unit;
    var t5y=traj[19]||traj[traj.length-1];
    var pct=((t5y.p50/base)-1)*100;
    var cls=pct>1?"up":pct<-1?"down":"flat";
    var arrow=pct>1?"&#9650;":pct<-1?"&#9660;":"&#9654;";
    var color=COLORS[asset]||"#94a3b8";
    container.innerHTML+='<div class="card"><div class="name" style="color:'+color+'">'+asset.replace("_"," ")+'</div>'
      +'<div class="price" style="color:'+color+'">'+t5y.p50.toFixed(2)+'</div>'
      +'<div class="change '+cls+'">'+arrow+" "+pct.toFixed(1)+'% from '+base.toFixed(2)+' '+unit+'</div>'
      +'<div class="range">P10: '+t5y.p10.toFixed(2)+' &mdash; P90: '+t5y.p90.toFixed(2)+'</div></div>';
  }
}

function buildLegend(){
  var container=document.getElementById("legend");
  container.innerHTML="";
  for(var a=0;a<ASSETS.length;a++){
    var asset=ASSETS[a];
    var color=COLORS[asset]||"#94a3b8";
    var div=document.createElement("div");
    div.className="legend-item"+(VISIBLE[asset]?"":" hidden");
    div.innerHTML='<span class="legend-dot" style="background:'+color+'"></span>'+asset.replace("_"," ");
    div.dataset.asset=asset;
    div.addEventListener("click",function(){
      var a=this.dataset.asset;
      VISIBLE[a]=!VISIBLE[a];
      this.classList.toggle("hidden");
      renderAll();
    });
    container.appendChild(div);
  }
}

function renderAll(){
  updateLabels();
  drawChart();
  updateCards();
}

// Slider events.
["s-shock","s-duration","s-demand","s-geo"].forEach(function(id){
  document.getElementById(id).addEventListener("input",renderAll);
});

// Stance buttons.
document.querySelectorAll(".stance-btn").forEach(function(btn){
  btn.addEventListener("click",function(){
    document.querySelectorAll(".stance-btn").forEach(function(b){b.classList.remove("active");});
    this.classList.add("active");
    STANCE=this.dataset.stance;
    renderAll();
  });
});

// Tooltip on hover.
var chartEl=document.getElementById("chart");
chartEl.addEventListener("mousemove",function(e){
  if(!window._chartMeta||!DATA)return;
  var m=window._chartMeta;
  var rect=chartEl.getBoundingClientRect();
  var mx=e.clientX-rect.left;
  var my=e.clientY-rect.top;
  if(mx<m.pad.l||mx>m.W-m.pad.r||my<m.pad.t||my>m.H-m.pad.b){
    document.getElementById("tooltip").style.display="none";return;
  }
  var qi=Math.round((mx-m.pad.l)/m.cw*(m.n-1));
  qi=Math.max(0,Math.min(m.n-1,qi));
  var html="<b>"+((m.series[ASSETS[0]]||{labels:[]}).labels[qi]||"")+"</b><br>";
  for(var a=0;a<ASSETS.length;a++){
    var asset=ASSETS[a];
    if(!VISIBLE[asset]||!m.series[asset])continue;
    var color=COLORS[asset]||"#94a3b8";
    html+='<span style="color:'+color+'">&#9679;</span> '+asset.replace("_"," ")+": "
      +m.series[asset].pcts[qi].toFixed(1)+"% "
      +"<span style='color:#64748b'>("+m.series[asset].p10s[qi].toFixed(1)+" to "+m.series[asset].p90s[qi].toFixed(1)+"%)</span><br>";
  }
  var tip=document.getElementById("tooltip");
  tip.innerHTML=html;tip.style.display="block";
  var tx=mx+12,ty=my-10;
  if(tx+180>m.W)tx=mx-180;
  tip.style.left=tx+"px";tip.style.top=ty+"px";
});
chartEl.addEventListener("mouseleave",function(){
  document.getElementById("tooltip").style.display="none";
});

// Resize handler.
window.addEventListener("resize",function(){if(DATA)drawChart();});

// postMessage listener.
window.addEventListener("message",function(e){
  if(e.source!==window.parent)return;
  var d=e.data;
  if(!d||d.type!=="mcp-app-result")return;
  if(d.tool!=="forecast-impact")return;
  try{DATA=JSON.parse(d.result);}catch(err){return;}

  ASSETS=Object.keys(DATA.assets);
  for(var a=0;a<ASSETS.length;a++)VISIBLE[ASSETS[a]]=true;

  // Set slider initial values from input_parameters.
  var ip=DATA.input_parameters;
  document.getElementById("s-shock").value=ip.supply_shock_severity;
  document.getElementById("s-duration").value=ip.disruption_months;
  document.getElementById("s-demand").value=Math.round(ip.demand_growth_rate*10);
  document.getElementById("s-geo").value=ip.geopolitical_risk;

  // Set stance button.
  STANCE=ip.central_bank_stance||"neutral";
  document.querySelectorAll(".stance-btn").forEach(function(b){
    b.classList.toggle("active",b.dataset.stance===STANCE);
  });

  document.getElementById("subtitle").textContent=DATA.scenario||"Financial Impact Forecast";
  document.getElementById("waiting").style.display="none";
  document.getElementById("dashboard").style.display="block";

  buildLegend();
  updateHeatmap();
  renderAll();
});
})();
</script>
</body>
</html>'''


def market_context_html():
    """Simple market data table HTML for fetch-market-context."""
    return '''<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Market Context</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f172a;color:#e2e8f0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:16px;font-size:13px}
h1{font-size:16px;font-weight:600;color:#f8fafc;margin-bottom:4px}
.sub{font-size:11px;color:#94a3b8;margin-bottom:12px}
table{width:100%;border-collapse:collapse}
th{text-align:left;padding:8px;color:#94a3b8;font-size:11px;font-weight:500;border-bottom:1px solid #334155}
td{padding:8px;border-bottom:1px solid #1e293b;font-size:12px}
.up{color:#4ade80}.down{color:#f87171}.flat{color:#94a3b8}
.range-bar{height:6px;background:#1e293b;border-radius:3px;position:relative;min-width:80px}
.range-fill{height:100%;background:#334155;border-radius:3px;position:absolute;top:0}
.range-dot{width:8px;height:8px;background:#38bdf8;border-radius:50%;position:absolute;top:-1px;transform:translateX(-50%)}
.waiting{text-align:center;padding:40px;color:#64748b}
</style>
</head>
<body>
<h1>Current Market Data</h1>
<div class="sub" id="subtitle">Waiting for data...</div>
<div id="waiting" class="waiting">Run fetch-market-context to populate this table.</div>
<table id="table" style="display:none">
<thead><tr><th>Commodity</th><th>Price</th><th>52-Week Range</th><th>Volatility</th><th>Trend</th></tr></thead>
<tbody id="tbody"></tbody>
</table>
<script>
window.addEventListener("message",function(e){
  if(e.source!==window.parent)return;
  var d=e.data;if(!d||d.type!=="mcp-app-result")return;
  if(d.tool!=="fetch-market-context")return;
  var data;try{data=JSON.parse(d.result);}catch(err){return;}
  var c=data.commodities;if(!c)return;
  document.getElementById("waiting").style.display="none";
  document.getElementById("table").style.display="table";
  document.getElementById("subtitle").textContent="Last updated: "+(data.last_updated||"N/A");
  var tbody=document.getElementById("tbody");tbody.innerHTML="";
  var keys=Object.keys(c);
  for(var i=0;i<keys.length;i++){
    var name=keys[i],v=c[name];
    if(v.error){tbody.innerHTML+="<tr><td>"+name+"</td><td colspan=4 style='color:#f87171'>"+v.error+"</td></tr>";continue;}
    var cls=v.trend==="up"?"up":v.trend==="down"?"down":"flat";
    var arrow=v.trend==="up"?"&#9650;":v.trend==="down"?"&#9660;":"&#9654;";
    var range52=v["52w_high"]-v["52w_low"];
    var pos=range52>0?((v.current_price-v["52w_low"])/range52*100):50;
    var fillW=range52>0?((v["52w_high"]-v["52w_low"])/v["52w_high"]*100):50;
    tbody.innerHTML+="<tr><td style='font-weight:600'>"+name.replace("_"," ")+"</td>"
      +"<td>"+v.current_price+" "+v.unit+"</td>"
      +"<td><div style='display:flex;align-items:center;gap:6px'><span style='font-size:10px;color:#64748b'>"+v["52w_low"]+"</span>"
      +"<div class='range-bar' style='flex:1'><div class='range-fill' style='width:100%'></div><div class='range-dot' style='left:"+pos+"%'></div></div>"
      +"<span style='font-size:10px;color:#64748b'>"+v["52w_high"]+"</span></div></td>"
      +"<td>"+v.volatility_30d+"%</td>"
      +"<td class='"+cls+"'>"+arrow+"</td></tr>";
  }
});
</script>
</body>
</html>'''


# ── Tool definitions ─────────────────────────────────────────────────────────

TOOLS = [
    {
        "name": "forecast-impact",
        "description": (
            "Run a multi-factor financial impact forecast for a given scenario. "
            "Returns projected price trajectories with confidence intervals for "
            "multiple correlated assets over 1-10 years, plus factor sensitivity data. "
            "The UI renders an interactive dashboard with sliders to adjust assumptions."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "scenario": {
                    "type": "string",
                    "description": "Natural-language description of the event or decision to analyze.",
                },
                "primary_commodity": {
                    "type": "string",
                    "description": "The primary commodity or asset to focus on (e.g., 'wheat', 'gold', 'crude_oil').",
                },
                "supply_shock_severity": {
                    "type": "number",
                    "description": "Severity of supply disruption as percentage (0-100). Default: 50.",
                },
                "disruption_months": {
                    "type": "integer",
                    "description": "Duration of the disruption in months (1-60). Default: 12.",
                },
                "demand_growth_rate": {
                    "type": "number",
                    "description": "Assumed annual global demand growth rate as percentage (-5 to 10). Default: 2.0.",
                },
                "central_bank_stance": {
                    "type": "string",
                    "enum": ["dovish", "neutral", "hawkish"],
                    "description": "Assumed central bank monetary policy stance. Default: 'neutral'.",
                },
                "geopolitical_risk": {
                    "type": "number",
                    "description": "Geopolitical risk multiplier (0-100). Default: 30.",
                },
                "context_data": {
                    "type": "string",
                    "description": "Optional enrichment context from RAG search and market data (JSON string).",
                },
            },
            "required": ["scenario"],
        },
        "_meta": {"ui": {"resourceUri": "financial://forecast-dashboard"}},
    },
    {
        "name": "fetch-market-context",
        "description": (
            "Fetch current market and economic baseline data for specified commodities or sectors. "
            "Returns current prices, 52-week high/low, volatility, and trend direction."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "commodities": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "List of commodity/asset names (e.g., ['wheat', 'gold', 'crude_oil', 'usd_index']).",
                },
            },
            "required": ["commodities"],
        },
        "_meta": {"ui": {"resourceUri": "financial://market-context"}},
    },
]


# ── JSON-RPC 2.0 handler ────────────────────────────────────────────────────


class MCPHandler(BaseHTTPRequestHandler):
    """Minimal JSON-RPC 2.0 handler implementing MCP HTTP transport."""

    def do_GET(self):
        """Health check endpoint."""
        if self.path == "/healthz":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._respond({"error": {"code": -32700, "message": "Parse error"}}, None)
            return
        self.handle_rpc(req)

    def handle_rpc(self, req):
        rid = req.get("id")
        method = req.get("method", "")
        params = req.get("params", {})

        if method == "initialize":
            self._respond({
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {"listChanged": False}, "resources": {"subscribe": False}},
                "serverInfo": {"name": "financial-mcp-server", "version": "0.1.0"},
            }, rid)

        elif method == "notifications/initialized":
            self.send_response(204)
            self.end_headers()

        elif method == "tools/list":
            self._respond({"tools": TOOLS}, rid)

        elif method == "tools/call":
            name = params.get("name", "")
            args = params.get("arguments", {})
            if name == "forecast-impact":
                result = forecast_impact(args)
                text = json.dumps(result)
            elif name == "fetch-market-context":
                result = fetch_market_context(args)
                text = json.dumps(result)
            else:
                self._respond({
                    "content": [{"type": "text", "text": json.dumps({"error": f"Unknown tool: {name}"})}],
                    "isError": True,
                }, rid)
                return
            self._respond({
                "content": [{"type": "text", "text": text}],
                "isError": False,
            }, rid)

        elif method == "resources/read":
            uri = params.get("uri", "")
            if uri == "financial://forecast-dashboard":
                html = forecast_dashboard_html()
            elif uri == "financial://market-context":
                html = market_context_html()
            else:
                self._respond({
                    "contents": [{"uri": uri, "mimeType": "text/plain", "text": f"Unknown resource: {uri}"}]
                }, rid)
                return
            self._respond({
                "contents": [{"uri": uri, "mimeType": "text/html", "text": html}]
            }, rid)

        else:
            self._respond({"error": {"code": -32601, "message": f"Method not found: {method}"}}, rid)

    def _respond(self, result, rid):
        body = json.dumps({"jsonrpc": "2.0", "id": rid, "result": result})
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, fmt, *args):
        """Suppress default access logs; keep it quiet."""
        pass


if __name__ == "__main__":
    server = HTTPServer(("0.0.0.0", PORT), MCPHandler)
    print(f"financial-mcp-server listening on :{PORT}", flush=True)
    server.serve_forever()
