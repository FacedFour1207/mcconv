// Command gendata builds internal/translate/data/tables.json.gz, the block,
// item and biome tables mcconv converts with. Its inputs are all freely
// licensed or generated locally:
//
//   - Java block states: the game's own data generator report
//     (java -cp <client or server jar> net.minecraft.data.Main --reports)
//   - Java -> Bedrock block/item/biome mappings: GeyserMC/mappings (MIT)
//   - Bedrock biome ids and banner pattern codes: pmmp/BedrockData (CC0)
//   - Bedrock block state upgrade schemas: BedrockBlockUpgradeSchema (CC0)
//
// Usage:
//
//	go run ./tools/gendata -report blocks.json -java 26.2 -java-dv 4903 \
//	    -geyser mappings/ -pmmp BedrockData/ -upgrade nbt_upgrade_schema/ -o tables.json.gz
//
// mcconv update-data does the same at run time (see internal/update).
package main

import (
	"flag"
	"fmt"
	"os"

	"mcconv/internal/translate/gen"
)

func main() {
	var o gen.Options
	flag.StringVar(&o.Report, "report", "", "Java data generator reports/blocks.json")
	flag.StringVar(&o.JavaVersion, "java", "", "Java version name of the report (e.g. 26.2)")
	flag.IntVar(&o.JavaDataVersion, "java-dv", 0, "Java DataVersion of the report")
	flag.StringVar(&o.GeyserDir, "geyser", "", "GeyserMC/mappings checkout")
	flag.StringVar(&o.PmmpDir, "pmmp", "", "pmmp/BedrockData checkout")
	flag.StringVar(&o.UpgradeDir, "upgrade", "", "BedrockBlockUpgradeSchema nbt_upgrade_schema directory")
	out := flag.String("o", "internal/translate/data/tables.json.gz", "output file")
	flag.Parse()
	data, err := gen.Build(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(data))
}
