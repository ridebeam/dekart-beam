package dekart

import (
	"context"
	"database/sql"
	"dekart/src/proto"
	"dekart/src/server/user"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newUUID() string {
	u, err := uuid.NewRandom()
	if err != nil {
		log.Fatal().Err(err).Send()
	}
	return u.String()
}

func (s Server) getReport(ctx context.Context, reportID string) (*proto.Report, error) {
	claims := user.GetClaims(ctx)
	if claims == nil {
		log.Fatal().Msg("getReport require claims")
	}
	reportRows, err := s.db.QueryContext(ctx,
		`select
			id,
			case when map_config is null then '' else map_config end as map_config,
			case when title is null then 'Untitled' else title end as title,
			author_email = $2 as can_write
		from reports where id=$1 and not archived limit 1`,
		reportID,
		claims.Email,
	)
	if err != nil {
		log.Err(err).Send()
		return nil, err
	}
	defer reportRows.Close()
	report := &proto.Report{}

	for reportRows.Next() {
		err = reportRows.Scan(
			&report.Id,
			&report.MapConfig,
			&report.Title,
			&report.CanWrite,
		)
		if err != nil {
			log.Err(err).Send()
			return nil, err
		}
	}
	if report.Id == "" {
		return nil, nil // not found
	}
	return report, nil
}

// CreateReport implementation
func (s Server) CreateReport(ctx context.Context, req *proto.CreateReportRequest) (*proto.CreateReportResponse, error) {
	claims := user.GetClaims(ctx)
	if claims == nil {
		return nil, Unauthenticated
	}
	id := newUUID()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO reports (id, author_email) VALUES ($1, $2)",
		id,
		claims.Email,
	)
	if err != nil {
		log.Err(err).Send()
		return nil, err
	}
	res := &proto.CreateReportResponse{
		Report: &proto.Report{
			Id: id,
		},
	}
	return res, nil
}

func rollback(tx *sql.Tx) {
	err := tx.Rollback()
	if err != nil {
		log.Err(err).Send()
	}
}

func (s Server) commitReportWithQueries(ctx context.Context, report *proto.Report, queries []*proto.Query) error {
	claims := user.GetClaims(ctx)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})

	_, err = tx.ExecContext(ctx,
		"INSERT INTO reports (id, author_email, map_config, title) VALUES ($1, $2, $3, $4)",
		report.Id,
		claims.Email,
		report.MapConfig,
		report.Title,
	)
	if err != nil {
		rollback(tx)
		return err
	}
	for _, query := range queries {
		queryId := newUUID()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO queries (id, report_id, query_text) VALUES($1, $2, $3)`,
			queryId,
			report.Id,
			query.QueryText,
		)
		if err != nil {
			rollback(tx)
			return err
		}
	}
	err = tx.Commit()
	return err
}

func (s Server) ForkReport(ctx context.Context, req *proto.ForkReportRequest) (*proto.ForkReportResponse, error) {
	claims := user.GetClaims(ctx)
	if claims == nil {
		return nil, Unauthenticated
	}

	_, err := uuid.Parse(req.ReportId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	reportID := newUUID()

	report, err := s.getReport(ctx, req.ReportId)
	if err != nil {
		log.Err(err).Send()
		return nil, err
	}
	if report == nil {
		err := fmt.Errorf("Report %s not found", reportID)
		log.Warn().Err(err).Send()
		return nil, status.Errorf(codes.NotFound, err.Error())
	}
	report.Id = reportID
	report.Title = fmt.Sprintf("Fork of %s", report.Title)

	sourceQueries, err := s.getQueries(ctx, req.ReportId)
	if err != nil {
		log.Err(err).Send()
		return nil, err
	}

	err = s.commitReportWithQueries(ctx, report, sourceQueries)
	if err != nil {
		log.Err(err).Send()
		return nil, err
	}

	return &proto.ForkReportResponse{
		ReportId: reportID,
	}, nil
}

// UpdateReport implementation
func (s Server) UpdateReport(ctx context.Context, req *proto.UpdateReportRequest) (*proto.UpdateReportResponse, error) {
	claims := user.GetClaims(ctx)
	if claims == nil {
		return nil, Unauthenticated
	}
	//start here, save queries
	if req.Report == nil {
		return nil, status.Errorf(codes.InvalidArgument, "req.Report == nil")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Err(err).Send()
		return nil, status.Error(codes.Internal, err.Error())
	}
	result, err := tx.ExecContext(ctx,
		`update
			reports
		set map_config=$1, title=$2
		where id=$3 and author_email=$4`,
		req.Report.MapConfig,
		req.Report.Title,
		req.Report.Id,
		claims.Email,
	)
	if err != nil {
		log.Err(err).Send()
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			log.Err(rollbackErr).Send()
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	affectedRows, err := result.RowsAffected()

	if err != nil {
		log.Err(err).Send()
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			log.Err(rollbackErr).Send()
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	if affectedRows == 0 {
		// TODO: distinguish between not found and read only
		err := fmt.Errorf("Report not found id:%s", req.Report.Id)
		log.Warn().Err(err).Send()
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			log.Err(rollbackErr).Send()
		}
		return nil, status.Error(codes.NotFound, err.Error())
	}

	// save queries
	for _, query := range req.Query {
		_, err = tx.ExecContext(ctx,
			`update queries set query_text=$1 where id=$2`,
			query.QueryText,
			query.Id,
		)
		if err != nil {
			log.Err(err).Send()
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Err(rollbackErr).Send()
			}
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	err = tx.Commit()

	if err != nil {
		log.Err(err).Send()
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.reportStreams.Ping(req.Report.Id)

	return &proto.UpdateReportResponse{}, nil
}

// ArchiveReport implementation
func (s Server) ArchiveReport(ctx context.Context, req *proto.ArchiveReportRequest) (*proto.ArchiveReportResponse, error) {
	claims := user.GetClaims(ctx)
	if claims == nil {
		return nil, Unauthenticated
	}
	_, err := uuid.Parse(req.ReportId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	result, err := s.db.ExecContext(ctx,
		"update reports set archived=$1 where id=$2 and author_email=$3",
		req.Archive,
		req.ReportId,
		claims.Email,
	)
	if err != nil {
		log.Err(err).Send()
		return nil, status.Error(codes.Internal, err.Error())
	}
	affectedRows, err := result.RowsAffected()

	if err != nil {
		log.Err(err).Send()
		return nil, status.Error(codes.Internal, err.Error())
	}

	if affectedRows == 0 {
		err := fmt.Errorf("Report not found id:%s", req.ReportId)
		log.Warn().Err(err).Send()
		return nil, status.Error(codes.NotFound, err.Error())
	}
	s.reportStreams.Ping(req.ReportId)

	return &proto.ArchiveReportResponse{}, nil

}
