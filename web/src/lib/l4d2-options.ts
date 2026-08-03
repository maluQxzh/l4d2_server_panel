export const gameModes = [
  { value: "coop", label: "战役" },
  { value: "realism", label: "写实" },
  { value: "versus", label: "对抗" },
  { value: "survival", label: "生还者" },
  { value: "scavenge", label: "清道夫" },
  { value: "mutation4", label: "绝境求生" },
] as const;

export const officialMapGroups = [
  { campaign: "死亡中心", maps: [["c1m1_hotel", "酒店"], ["c1m2_streets", "街道"], ["c1m3_mall", "购物中心"], ["c1m4_atrium", "中庭"]] },
  { campaign: "黑色狂欢节", maps: [["c2m1_highway", "公路"], ["c2m2_fairgrounds", "游乐场"], ["c2m3_coaster", "过山车"], ["c2m4_barns", "谷仓"], ["c2m5_concert", "音乐会"]] },
  { campaign: "沼泽激战", maps: [["c3m1_plankcountry", "乡村"], ["c3m2_swamp", "沼泽"], ["c3m3_shantytown", "贫民窟"], ["c3m4_plantation", "种植园"]] },
  { campaign: "暴风骤雨", maps: [["c4m1_milltown_a", "密尔城"], ["c4m2_sugarmill_a", "糖厂"], ["c4m3_sugarmill_b", "逃离糖厂"], ["c4m4_milltown_b", "重返密尔城"], ["c4m5_milltown_escape", "逃离小镇"]] },
  { campaign: "教区", maps: [["c5m1_waterfront", "码头"], ["c5m2_park", "公园"], ["c5m3_cemetery", "墓地"], ["c5m4_quarter", "特区"], ["c5m5_bridge", "大桥"]] },
  { campaign: "消逝", maps: [["c6m1_riverbank", "河岸"], ["c6m2_bedlam", "地下通道"], ["c6m3_port", "港口"]] },
  { campaign: "牺牲", maps: [["c7m1_docks", "码头"], ["c7m2_barge", "驳船"], ["c7m3_port", "港口"]] },
  { campaign: "毫不留情", maps: [["c8m1_apartment", "公寓"], ["c8m2_subway", "地铁"], ["c8m3_sewers", "下水道"], ["c8m4_interior", "医院"], ["c8m5_rooftop", "屋顶"]] },
  { campaign: "坠机险途", maps: [["c9m1_alleys", "巷道"], ["c9m2_lots", "卡车停车场"]] },
  { campaign: "死亡丧钟", maps: [["c10m1_caves", "收费公路"], ["c10m2_drainage", "水沟"], ["c10m3_ranchhouse", "教堂"], ["c10m4_mainstreet", "小镇"], ["c10m5_houseboat", "船库"]] },
  { campaign: "寂静时分", maps: [["c11m1_greenhouse", "温室"], ["c11m2_offices", "起重机"], ["c11m3_garage", "建筑工地"], ["c11m4_terminal", "终点站"], ["c11m5_runway", "跑道"]] },
  { campaign: "血腥收获", maps: [["c12m1_hilltop", "森林"], ["c12m2_traintunnel", "隧道"], ["c12m3_bridge", "桥"], ["c12m4_barn", "火车站"], ["c12m5_cornfield", "农舍"]] },
  { campaign: "刺骨寒溪", maps: [["c13m1_alpinecreek", "高山溪流"], ["c13m2_southpinestream", "南方松林"], ["c13m3_memorialbridge", "纪念大桥"], ["c13m4_cutthroatcreek", "残酷溪流"]] },
  { campaign: "临死一搏", maps: [["c14m1_junkyard", "废料场"], ["c14m2_lighthouse", "灯塔"]] },
] as const;

export const defaultTickrate = 100;
export const minTickrate = 1;
export const maxTickrate = 1000;
