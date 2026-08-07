ALTER TABLE maps ADD COLUMN author TEXT NOT NULL DEFAULT ''
    CHECK (length(author)<=255 AND instr(author,char(0))=0 AND instr(author,char(10))=0 AND instr(author,char(13))=0);
ALTER TABLE maps ADD COLUMN version TEXT NOT NULL DEFAULT ''
    CHECK (length(version)<=64 AND instr(version,char(0))=0 AND instr(version,char(10))=0 AND instr(version,char(13))=0);
ALTER TABLE maps ADD COLUMN description TEXT NOT NULL DEFAULT ''
    CHECK (length(description)<=2048 AND instr(description,char(0))=0 AND instr(description,char(10))=0 AND instr(description,char(13))=0);
ALTER TABLE maps ADD COLUMN poster_mime TEXT NOT NULL DEFAULT ''
    CHECK (poster_mime IN ('','image/jpeg'));
ALTER TABLE maps ADD COLUMN poster_data BLOB NOT NULL DEFAULT X''
    CHECK (length(poster_data)<=1048576 AND ((poster_mime='' AND length(poster_data)=0) OR (poster_mime='image/jpeg' AND length(poster_data)>0)));

CREATE TABLE map_modes (
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    ordinal INTEGER NOT NULL CHECK (ordinal>=0 AND ordinal<64),
    PRIMARY KEY(map_id,name),
    UNIQUE(map_id,ordinal)
);

CREATE TABLE map_chapters (
    map_id TEXT NOT NULL,
    mode TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 4096),
    bsp_name TEXT NOT NULL CHECK (length(bsp_name) BETWEEN 1 AND 128),
    display_name TEXT NOT NULL CHECK (length(display_name) BETWEEN 1 AND 255),
    image TEXT NOT NULL DEFAULT '' CHECK (length(image)<=512),
    PRIMARY KEY(map_id,mode,ordinal),
    FOREIGN KEY(map_id,mode) REFERENCES map_modes(map_id,name) ON DELETE CASCADE
);
CREATE INDEX map_chapters_map_idx ON map_chapters(map_id,mode,ordinal);
