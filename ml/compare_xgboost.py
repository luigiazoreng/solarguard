#!/usr/bin/env python3
"""Offline XGBoost comparison against SolarGuard Go models.

Evaluates:
- XGBoost walk-forward (expanding window, refit every 7 days, 60 days minimum)
- XGBoost 80/20 chronological (no shuffle)
- xgboost_old_alignment (+1 day weather shift) to illustrate the impact of the date alignment bug.
"""

import argparse
import json
import math
import os
import sqlite3
import sys
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

import numpy as np
import pandas as pd
import xgboost as xgb


def parse_args():
    parser = argparse.ArgumentParser(description="SolarGuard XGBoost offline comparison")
    parser.add_argument("--db", required=True, help="Path to SQLite database")
    parser.add_argument("--plant-id", type=int, required=True, help="Plant ID")
    parser.add_argument("--out", required=True, help="Output path for results.json")
    return parser.parse_args()


def compute_metrics(actuals: np.ndarray, preds: np.ndarray) -> dict:
    n = len(actuals)
    if n == 0 or len(preds) != n:
        return {"n": 0, "mae": 0.0, "rmse": 0.0, "r2": 0.0, "wape": 0.0, "mape": None, "mape_n": 0, "bias": 0.0}

    diff = preds - actuals
    abs_diff = np.abs(diff)

    mae = float(np.mean(abs_diff))
    rmse = float(np.sqrt(np.mean(diff ** 2)))
    bias = float(np.mean(diff))

    sum_act = float(np.sum(actuals))
    wape = float(np.sum(abs_diff) / sum_act) if sum_act > 0 else 0.0

    mean_y = float(np.mean(actuals))
    ss_tot = float(np.sum((actuals - mean_y) ** 2))
    ss_res = float(np.sum(diff ** 2))
    if ss_tot > 0:
        r2 = float(1.0 - (ss_res / ss_tot))
    elif ss_res == 0:
        r2 = 1.0
    else:
        r2 = 0.0

    # MAPE for actuals > 5.0 kWh
    mask_5 = actuals > 5.0
    mape_n = int(np.sum(mask_5))
    if mape_n > 0:
        mape_val = float(np.mean(abs_diff[mask_5] / actuals[mask_5]) * 100.0)
    else:
        mape_val = None

    return {
        "n": n,
        "mae": mae,
        "rmse": rmse,
        "r2": r2,
        "wape": wape,
        "mape": mape_val,
        "mape_n": mape_n,
        "bias": bias,
    }


def format_table(title: str, metrics_map: dict) -> str:
    lines = [
        title,
        f"{'Modelo':<24} {'N':>6} {'MAE (kWh)':>10} {'RMSE (kWh)':>10} {'R²':>8} {'WAPE':>9} {'MAPE (>5kWh)':>20} {'Bias (kWh)':>11}",
        "-" * 96,
    ]
    for name, m in metrics_map.items():
        mape_str = "-"
        if m.get("mape") is not None:
            mape_str = f"{m['mape']:6.2f}% (n={m['mape_n']})"
        lines.append(
            f"{name:<24} {m['n']:>6} {m['mae']:>10.3f} {m['rmse']:>10.3f} {m['r2']:>8.4f} {m['wape']*100.0:>8.2f}% {mape_str:>20} {m['bias']:>+11.3f}"
        )
    lines.append("-" * 96)
    return "\n".join(lines)


def train_and_predict_xgb(X_train: np.ndarray, y_train: np.ndarray, X_test: np.ndarray) -> np.ndarray:
    dtrain = xgb.DMatrix(X_train, label=y_train)
    dtest = xgb.DMatrix(X_test)
    params = {
        "max_depth": 3,
        "learning_rate": 0.05,
        "subsample": 0.8,
        "seed": 42,
        "objective": "reg:squarederror",
    }
    bst = xgb.train(params, dtrain, num_boost_round=200)
    preds = bst.predict(dtest)
    return np.maximum(0.0, preds)


def impute_features(train_X: np.ndarray, test_X: np.ndarray):
    """Impute NaNs with column means calculated from train_X only."""
    train_imputed = train_X.copy()
    test_imputed = test_X.copy()
    for col in range(train_X.shape[1]):
        col_vals = train_X[:, col]
        valid = col_vals[~np.isnan(col_vals)]
        mean_val = float(np.mean(valid)) if len(valid) > 0 else 0.0

        train_imputed[np.isnan(train_imputed[:, col]), col] = mean_val
        test_imputed[np.isnan(test_imputed[:, col]), col] = mean_val
    return train_imputed, test_imputed


def evaluate_series(df: pd.DataFrame, min_train_days=60, refit_days=7):
    """Runs walk-forward and 80/20 for one device DataFrame.

    Features: [irradiation, temp_mean, precipitation, humidity]
    Target: y
    """
    feature_cols = ["irradiation", "temp_mean", "precipitation", "humidity"]
    n = len(df)
    if n < min_train_days:
        return None

    # 1. Walk-Forward
    wf_actuals = []
    wf_preds = []

    for train_end in range(min_train_days, n, refit_days):
        test_end = min(train_end + refit_days, n)
        train_df = df.iloc[0:train_end]
        test_df = df.iloc[train_end:test_end]

        X_train_raw = train_df[feature_cols].values.astype(float)
        y_train = train_df["y"].values.astype(float)
        X_test_raw = test_df[feature_cols].values.astype(float)
        y_test = test_df["y"].values.astype(float)

        X_train, X_test = impute_features(X_train_raw, X_test_raw)
        preds = train_and_predict_xgb(X_train, y_train, X_test)

        wf_actuals.extend(y_test)
        wf_preds.extend(preds)

    wf_metrics = compute_metrics(np.array(wf_actuals), np.array(wf_preds))

    # 2. 80/20 Chronological
    split_idx = int(n * 0.8)
    if split_idx < 1:
        split_idx = 1
    train80 = df.iloc[0:split_idx]
    test20 = df.iloc[split_idx:]

    X_train_raw = train80[feature_cols].values.astype(float)
    y_train = train80["y"].values.astype(float)
    X_test_raw = test20[feature_cols].values.astype(float)
    y_test = test20["y"].values.astype(float)

    X_train, X_test = impute_features(X_train_raw, X_test_raw)
    preds8020 = train_and_predict_xgb(X_train, y_train, X_test)

    s8020_metrics = compute_metrics(y_test, preds8020)

    return {
        "wf": {"actuals": wf_actuals, "preds": wf_preds, "metrics": wf_metrics},
        "8020": {"actuals": list(y_test), "preds": list(preds8020), "metrics": s8020_metrics},
    }


def main():
    args = parse_args()
    db_abs = os.path.abspath(args.db)
    if not os.path.exists(db_abs):
        sys.exit(f"Erro: banco de dados {db_abs} não existe")

    # Read-only connection to SQLite
    db_uri = f"file:{db_abs}?mode=ro"
    conn = sqlite3.connect(db_uri, uri=True)
    cur = conn.cursor()

    cur.execute("SELECT id, name, timezone, weather_location_id FROM plants WHERE id = ?", (args.plant_id,))
    plant_row = cur.fetchone()
    if not plant_row:
        sys.exit(f"Erro: planta {args.plant_id} não encontrada")

    plant_id, plant_name, plant_tz, weather_loc_id = plant_row
    if not weather_loc_id:
        sys.exit(f"Erro: planta {plant_id} não possui local de clima")

    tz = ZoneInfo(plant_tz)

    cur.execute(
        "SELECT id, name FROM devices WHERE plant_id = ? AND LOWER(kind) != 'plant' ORDER BY name ASC",
        (plant_id,),
    )
    devices = cur.fetchall()
    if not devices:
        sys.exit(f"Erro: nenhum dispositivo de geração para planta {plant_id}")

    # Load weather preference: inmet > open-meteo (matching Store.PreferredObserved)
    cur.execute(
        """SELECT date, provider, irradiation_kwh_m2, temperature_mean_c, precipitation_mm, humidity_mean_pct
           FROM weather_days
           WHERE location_id = ? AND kind = 'observed'
           ORDER BY date ASC""",
        (weather_loc_id,),
    )
    weather_rows = cur.fetchall()

    rank = {"inmet": 0, "open-meteo": 1}
    best_weather = {}
    for date_str, provider, irr, tmean, precip, hum in weather_rows:
        if provider not in rank:
            continue
        r = rank[provider]
        if date_str not in best_weather or r < best_weather[date_str]["rank"]:
            best_weather[date_str] = {
                "rank": r,
                "irradiation": float(irr) if irr is not None else None,
                "temp_mean": float(tmean) if tmean is not None else np.nan,
                "precipitation": float(precip) if precip is not None else np.nan,
                "humidity": float(hum) if hum is not None else np.nan,
            }

    # "A day missing irradiation is skipped"
    weather_map = {
        d: v for d, v in best_weather.items() if v["irradiation"] is not None
    }

    # Evaluate each device
    dev_results_correct = {}
    dev_results_old = {}

    pooled_wf_correct_act = []
    pooled_wf_correct_prd = []
    pooled_8020_correct_act = []
    pooled_8020_correct_prd = []

    pooled_wf_old_act = []
    pooled_wf_old_prd = []
    pooled_8020_old_act = []
    pooled_8020_old_prd = []

    for dev_id, dev_name in devices:
        cur.execute(
            """SELECT period_start, value
               FROM readings
               WHERE device_id = ? AND metric = 'energy_kwh' AND quality = 'ok' AND value IS NOT NULL
               ORDER BY period_start ASC""",
            (dev_id,),
        )
        readings = cur.fetchall()

        rows_correct = []
        rows_old = []

        for pstart, val in readings:
            # Parse period_start to local date
            dt = datetime.fromisoformat(pstart.replace("Z", "+00:00")).astimezone(tz)
            date_str = dt.strftime("%Y-%m-%d")

            # Correct alignment: weather on date_str
            if date_str in weather_map:
                w = weather_map[date_str]
                rows_correct.append({
                    "date": date_str,
                    "y": float(val),
                    "irradiation": w["irradiation"],
                    "temp_mean": w["temp_mean"],
                    "precipitation": w["precipitation"],
                    "humidity": w["humidity"],
                })

            # Old alignment: weather shifted by +1 (as if matched directly against unshifted UTC date stamp)
            date_obj = datetime.strptime(date_str, "%Y-%m-%d").date()
            old_date_str = (date_obj + timedelta(days=1)).strftime("%Y-%m-%d")
            if old_date_str in weather_map:
                w = weather_map[old_date_str]
                rows_old.append({
                    "date": date_str,
                    "y": float(val),
                    "irradiation": w["irradiation"],
                    "temp_mean": w["temp_mean"],
                    "precipitation": w["precipitation"],
                    "humidity": w["humidity"],
                })

        df_correct = pd.DataFrame(rows_correct)
        df_old = pd.DataFrame(rows_old)

        res_c = evaluate_series(df_correct)
        res_o = evaluate_series(df_old)

        if res_c:
            dev_results_correct[dev_name] = res_c
            pooled_wf_correct_act.extend(res_c["wf"]["actuals"])
            pooled_wf_correct_prd.extend(res_c["wf"]["preds"])
            pooled_8020_correct_act.extend(res_c["8020"]["actuals"])
            pooled_8020_correct_prd.extend(res_c["8020"]["preds"])

        if res_o:
            dev_results_old[dev_name] = res_o
            pooled_wf_old_act.extend(res_o["wf"]["actuals"])
            pooled_wf_old_prd.extend(res_o["wf"]["preds"])
            pooled_8020_old_act.extend(res_o["8020"]["actuals"])
            pooled_8020_old_prd.extend(res_o["8020"]["preds"])

    conn.close()

    pooled_wf_c_metrics = compute_metrics(np.array(pooled_wf_correct_act), np.array(pooled_wf_correct_prd))
    pooled_wf_o_metrics = compute_metrics(np.array(pooled_wf_old_act), np.array(pooled_wf_old_prd))
    pooled_8020_c_metrics = compute_metrics(np.array(pooled_8020_correct_act), np.array(pooled_8020_correct_prd))
    pooled_8020_o_metrics = compute_metrics(np.array(pooled_8020_old_act), np.array(pooled_8020_old_prd))

    # Print output matching Go layout
    print("\n" + "=" * 96)
    print(f"XGBoost Offline Comparison - Planta {plant_id}: {plant_name}")
    print("=" * 96)

    wf_pooled_table = {
        "xgboost": pooled_wf_c_metrics,
        "xgboost_old_alignment": pooled_wf_o_metrics,
    }
    print(format_table("[Walk-Forward (janela expansível)] - Agrupado (Pooled)", wf_pooled_table))
    print()

    # Per device walk-forward
    for dev_name in sorted(dev_results_correct.keys()):
        m_map = {
            "xgboost": dev_results_correct[dev_name]["wf"]["metrics"],
            "xgboost_old_alignment": dev_results_old[dev_name]["wf"]["metrics"],
        }
        print(format_table(f"[Walk-Forward] - Dispositivo: {dev_name}", m_map))
        print()

    # 80/20 Pooled
    split_pooled_table = {
        "xgboost": pooled_8020_c_metrics,
        "xgboost_old_alignment": pooled_8020_o_metrics,
    }
    print(format_table("[Divisão 80/20 Cronológica] - Agrupado (Pooled)", split_pooled_table))
    print()

    for dev_name in sorted(dev_results_correct.keys()):
        m_map = {
            "xgboost": dev_results_correct[dev_name]["8020"]["metrics"],
            "xgboost_old_alignment": dev_results_old[dev_name]["8020"]["metrics"],
        }
        print(format_table(f"[Divisão 80/20] - Dispositivo: {dev_name}", m_map))
        print()

    # Save to out JSON
    out_dir = os.path.dirname(os.path.abspath(args.out))
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)

    results_data = {
        "plant_id": plant_id,
        "plant_name": plant_name,
        "walk_forward": {
            "pooled": {
                "xgboost": pooled_wf_c_metrics,
                "xgboost_old_alignment": pooled_wf_o_metrics,
            },
            "devices": {
                dev_name: {
                    "xgboost": dev_results_correct[dev_name]["wf"]["metrics"],
                    "xgboost_old_alignment": dev_results_old[dev_name]["wf"]["metrics"],
                }
                for dev_name in dev_results_correct
            },
        },
        "fixed_split_80_20": {
            "pooled": {
                "xgboost": pooled_8020_c_metrics,
                "xgboost_old_alignment": pooled_8020_o_metrics,
            },
            "devices": {
                dev_name: {
                    "xgboost": dev_results_correct[dev_name]["8020"]["metrics"],
                    "xgboost_old_alignment": dev_results_old[dev_name]["8020"]["metrics"],
                }
                for dev_name in dev_results_correct
            },
        },
    }

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(results_data, f, indent=2, ensure_ascii=False)

    print(f"Resultados salvos em: {args.out}")


if __name__ == "__main__":
    main()
